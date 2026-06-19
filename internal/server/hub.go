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

// heartbeatTimeout is how long the hub waits for any message before treating
// the worker as gone. Workers send heartbeats every 10 s; this gives 3 missed
// windows before eviction (phase 3 formalises the eviction policy).
const heartbeatTimeout = 30 * time.Second

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
	info WorkerInfo
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

	wid := newHubWorkerID()
	now := time.Now()
	hw := &hubWorker{
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

	// The connection now carries inference as well as heartbeats. The remote
	// worker owns the single write pump and the request multiplexing; this read
	// loop is the sole reader, dispatching heartbeats and routing inference
	// responses to it. Register one gateway route per served model so the
	// gateway dispatches matching requests over this connection; drop them when
	// the loop exits (the worker disconnected).
	rw := newRemoteWorker(conn)
	go rw.writePump()
	defer rw.close()

	if h.registry != nil {
		for _, sm := range join.Models {
			h.registry.RegisterModel(Model{Name: sm.Name, Exec: rw, ContextWindow: sm.ContextWindow})
		}
		defer func() {
			for _, sm := range join.Models {
				h.registry.UnregisterModel(sm.Name)
			}
		}()
	}

	for {
		conn.SetReadDeadline(time.Now().Add(heartbeatTimeout)) //nolint:errcheck
		_, data, err := conn.ReadMessage()
		if err != nil {
			return // timeout or disconnect; deferred cleanup removes the worker
		}
		var m wire.Message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Type == wire.MsgHeartbeat {
			h.mu.Lock()
			if w, ok := h.workers[wid]; ok {
				w.info.LastSeen = time.Now()
			}
			h.mu.Unlock()
			rw.sendAck(wire.MsgHeartbeatAck, wire.HeartbeatAckPayload{})
			continue
		}
		// Inference response (chunk/done/response/token_count/error): hand to the
		// remote worker to demux back to the waiting request goroutine.
		rw.route(m)
	}
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
