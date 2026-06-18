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

// Hub manages persistent outbound-initiated WebSocket connections from remote
// workers (ADR-0003, ADR-0007). It is safe for concurrent use.
type Hub struct {
	token string

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
	ConnectedAt time.Time
	LastSeen    time.Time
}

// NewHub creates a Hub that authenticates workers with the given join token.
func NewHub(token string) *Hub {
	return &Hub{
		token:   token,
		workers: make(map[string]*hubWorker),
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
			_ = h.sendMsg(conn, wire.HeartbeatAckPayload{})
		}
	}
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

// sendMsg infers the message type from the payload type and sends it.
func (h *Hub) sendMsg(conn *websocket.Conn, payload any) error {
	var typ wire.MessageType
	switch payload.(type) {
	case wire.JoinAckPayload:
		typ = wire.MsgJoinAck
	case wire.HeartbeatAckPayload:
		typ = wire.MsgHeartbeatAck
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
