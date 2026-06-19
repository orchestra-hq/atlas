package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/wire"
)

// heartbeatTimeout is how long the hub waits for any message before treating the
// worker as gone and tearing the connection down. Workers send heartbeats every
// 10 s, so this is N=3 missed windows. The teardown is also the backstop for a
// worker that crashed with its TCP connection lingering: it unblocks that
// connection's in-flight requests with ErrEngineUnavailable within this window
// instead of leaving them to hang until the client's own deadline. A var (not a
// const) so tests can shrink it; production code never reassigns it.
var heartbeatTimeout = 30 * time.Second

// ModelRegistry is the gateway's model table as the hub mutates it: a worker
// registers a route per served model when it joins and removes those routes
// when it drops, so the gateway serves remote models exactly as in-process ones.
type ModelRegistry interface {
	RegisterModel(Model)
	UnregisterModel(name string)
}

// Hub manages persistent outbound-initiated WebSocket connections from remote
// workers (ADR-0003, ADR-0007). It is safe for concurrent use.
type Hub struct {
	token    string
	registry ModelRegistry

	mu      sync.RWMutex
	workers map[string]*hubWorker
}

type hubWorker struct {
	info   WorkerInfo
	rw     *remoteWorker      // the connection's write pump + request demux
	served []wire.ServedModel // models to unregister when this worker leaves

	// unregOnce makes route teardown idempotent: drain and the final disconnect
	// both unregister this worker's models, and whichever runs first wins.
	unregOnce sync.Once
}

// WorkerInfo is the gateway-facing snapshot of a connected worker.
type WorkerInfo struct {
	ID          string
	Name        string
	Hardware    wire.Hardware
	Version     string
	Models      []string
	ConnectedAt time.Time
	LastSeen    time.Time
	// Draining is true once the worker has begun graceful shutdown (it sent a
	// drain, or the server is evicting it): no new requests route to it, but its
	// in-flight requests are still completing.
	Draining bool
}

// NewHub creates a Hub that authenticates workers with the given join token and
// registers each joining worker's served models in registry (the gateway). A
// nil registry is allowed for tests that exercise only the join/heartbeat path.
func NewHub(token string, registry ModelRegistry) *Hub {
	return &Hub{
		token:    token,
		registry: registry,
		workers:  make(map[string]*hubWorker),
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// HandleConnect is the HTTP handler for GET /workers/connect. It upgrades the
// connection to WebSocket, performs the join handshake, then loops on
// heartbeat messages until the connection drops or the read deadline fires.
func (h *Hub) HandleConnect(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade writes its own 4xx
	}
	defer func() { _ = conn.Close() }()

	// Bound every frame before the first read — the join frame is read pre-auth,
	// so an unbounded read here is an unauthenticated OOM vector.
	conn.SetReadLimit(wire.MaxFrameBytes)
	conn.SetReadDeadline(time.Now().Add(heartbeatTimeout)) //nolint:errcheck

	_, data, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var env wire.Message
	if err := json.Unmarshal(data, &env); err != nil || env.Type != wire.MsgJoin {
		_ = h.sendMsg(conn, wire.JoinAckPayload{Accepted: false, Reason: "expected join"})
		return
	}
	var join wire.JoinPayload
	if err := json.Unmarshal(env.Payload, &join); err != nil {
		_ = h.sendMsg(conn, wire.JoinAckPayload{Accepted: false, Reason: "bad join payload"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(join.Token), []byte(h.token)) != 1 {
		_ = h.sendMsg(conn, wire.JoinAckPayload{Accepted: false, Reason: "invalid token"})
		return
	}

	// The connection now carries inference as well as heartbeats. The remote
	// worker owns the single write pump and the request multiplexing; this read
	// loop is the sole reader, dispatching heartbeats and routing inference
	// responses to it.
	rw := newRemoteWorker(conn)
	go rw.writePump()
	defer rw.close()

	wid := newHubWorkerID()
	now := time.Now()
	hw := &hubWorker{
		rw:     rw,
		served: join.Models,
		info: WorkerInfo{
			ID:          wid,
			Name:        join.Name,
			Hardware:    join.Hardware,
			Version:     join.Version,
			Models:      servedNames(join.Models),
			ConnectedAt: now,
			LastSeen:    now,
		},
	}
	h.mu.Lock()
	h.workers[wid] = hw
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.workers, wid)
		h.mu.Unlock()
	}()

	if err := h.sendMsg(conn, wire.JoinAckPayload{Accepted: true, WorkerID: wid}); err != nil {
		return
	}

	// Register one gateway route per served model so the gateway dispatches
	// matching requests over this connection; drop them when the worker leaves —
	// whether it drains, disconnects, or times out (whichever comes first, via
	// unregOnce).
	for _, sm := range join.Models {
		h.registerModel(Model{Name: sm.Name, Exec: rw, ContextWindow: sm.ContextWindow})
	}
	defer h.unregisterModels(hw)

	for {
		conn.SetReadDeadline(time.Now().Add(heartbeatTimeout)) //nolint:errcheck
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Timeout or disconnect. Deferred cleanup removes the worker and
			// rw.close() unblocks every request still multiplexed on this
			// connection with ErrEngineUnavailable, so a silently dead worker
			// (crashed but TCP lingering) frees its in-flight requests within the
			// heartbeat-timeout window rather than hanging until the client gives up.
			return
		}
		var m wire.Message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case wire.MsgHeartbeat:
			h.mu.Lock()
			if w, ok := h.workers[wid]; ok {
				w.info.LastSeen = time.Now()
			}
			h.mu.Unlock()
			rw.sendControl(wire.MsgHeartbeatAck, wire.HeartbeatAckPayload{})
		case wire.MsgDrain:
			// The worker is leaving: stop routing new requests to it now, but keep
			// the connection open so its in-flight requests can finish. The worker
			// sends drain_ack once they drain, then disconnects.
			h.beginDraining(hw)
		case wire.MsgDrainAck:
			// The worker has finished draining and is disconnecting; let cleanup run.
			return
		default:
			// Inference response (chunk/done/response/token_count/error): hand to the
			// remote worker to demux back to the waiting request goroutine.
			rw.route(m)
		}
	}
}

// registerModel adds a served-model route to the gateway, if a registry is set.
func (h *Hub) registerModel(m Model) {
	if h.registry != nil {
		h.registry.RegisterModel(m)
	}
}

// unregisterModels removes a worker's routes from the gateway exactly once, so
// new requests stop resolving to it. Phase 4 re-keys routes by connection
// identity; until then this is name-keyed, so a worker reconnecting under the
// same model name relies on registerModel running after this (re-add wins).
func (h *Hub) unregisterModels(hw *hubWorker) {
	hw.unregOnce.Do(func() {
		if h.registry == nil {
			return
		}
		for _, sm := range hw.served {
			h.registry.UnregisterModel(sm.Name)
		}
	})
}

// beginDraining marks a worker as draining and removes its routes so no new
// requests are sent to it; its in-flight requests continue over the still-open
// connection. Idempotent — drain may arrive once from the worker and again from
// an operator remove.
func (h *Hub) beginDraining(hw *hubWorker) {
	h.mu.Lock()
	hw.info.Draining = true
	h.mu.Unlock()
	h.unregisterModels(hw)
}

// servedNames extracts the model names from a join's served-model list.
func servedNames(models []wire.ServedModel) []string {
	if len(models) == 0 {
		return nil
	}
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	return names
}

// HandleListWorkers serves GET /admin/workers with the current worker inventory.
func (h *Hub) HandleListWorkers(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	infos := make([]WorkerInfo, 0, len(h.workers))
	for _, hw := range h.workers {
		infos = append(infos, hw.info)
	}
	h.mu.RUnlock()

	type response struct {
		Workers []WorkerInfo `json:"workers"`
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{Workers: infos})
}

// HandleRemoveWorker serves POST /admin/workers/{id}/drain: it begins
// server-initiated graceful shutdown of the worker. 202 if the worker is
// connected, 404 otherwise.
func (h *Hub) HandleRemoveWorker(w http.ResponseWriter, r *http.Request) {
	if h.DrainWorker(r.PathValue("id")) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Error(w, "worker not found", http.StatusNotFound)
}

// DrainWorker begins server-initiated graceful shutdown of a worker: it stops
// routing new requests to it and sends a drain so the worker finishes its
// in-flight requests, sends drain_ack, and disconnects. Returns false if no
// worker with that id is connected.
func (h *Hub) DrainWorker(id string) bool {
	h.mu.RLock()
	hw, ok := h.workers[id]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	h.beginDraining(hw)
	hw.rw.sendControl(wire.MsgDrain, wire.DrainPayload{})
	return true
}

// Workers returns a point-in-time snapshot of all connected workers.
func (h *Hub) Workers() []WorkerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(h.workers))
	for _, hw := range h.workers {
		out = append(out, hw.info)
	}
	return out
}

// sendMsg sends a control-plane payload directly on conn. It is used only for
// the join handshake, before the remote worker's write pump takes over as the
// connection's sole writer (heartbeat acks and inference frames go through that).
func (h *Hub) sendMsg(conn *websocket.Conn, payload any) error {
	var typ wire.MessageType
	switch payload.(type) {
	case wire.JoinAckPayload:
		typ = wire.MsgJoinAck
	default:
		return fmt.Errorf("hub: unknown outbound payload type %T", payload)
	}
	msg, err := wire.Encode(typ, "", payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	return conn.WriteMessage(websocket.TextMessage, data)
}

func newHubWorkerID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "w_" + hex.EncodeToString(b[:])
}
