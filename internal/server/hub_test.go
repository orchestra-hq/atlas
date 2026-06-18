package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/wire"
)

const testToken = "test-token-abc"

// dialHub starts a test server that routes all connections to hub.HandleConnect
// and returns a connected WebSocket client.
func dialHub(t *testing.T, hub *server.Hub) *websocket.Conn {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendMsg(t *testing.T, conn *websocket.Conn, typ wire.MessageType, payload any) {
	t.Helper()
	msg, err := wire.Encode(typ, "", payload)
	if err != nil {
		t.Fatalf("encode %s: %v", typ, err)
	}
	data, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
}

func readMsg(t *testing.T, conn *websocket.Conn) wire.Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var msg wire.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg
}

func joinHub(t *testing.T, conn *websocket.Conn, name string) wire.JoinAckPayload {
	t.Helper()
	sendMsg(t, conn, wire.MsgJoin, wire.JoinPayload{
		Token:    testToken,
		Version:  "test",
		Name:     name,
		Hardware: wire.Hardware{Platform: "cpu", RAMBytes: 8 << 30},
	})
	ackEnv := readMsg(t, conn)
	if ackEnv.Type != wire.MsgJoinAck {
		t.Fatalf("got %q want join_ack", ackEnv.Type)
	}
	var ack wire.JoinAckPayload
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		t.Fatalf("unmarshal join_ack: %v", err)
	}
	return ack
}

func TestHub_join_accepted(t *testing.T) {
	hub := server.NewHub(testToken)
	conn := dialHub(t, hub)

	ack := joinHub(t, conn, "test-worker")
	if !ack.Accepted {
		t.Fatalf("join rejected: %s", ack.Reason)
	}
	if ack.WorkerID == "" {
		t.Error("worker_id is empty")
	}

	// Allow handler goroutine to register the worker before checking.
	time.Sleep(20 * time.Millisecond)
	workers := hub.Workers()
	if len(workers) != 1 {
		t.Fatalf("hub has %d workers, want 1", len(workers))
	}
	if workers[0].Name != "test-worker" {
		t.Errorf("worker name = %q, want test-worker", workers[0].Name)
	}
	if workers[0].Hardware.Platform != "cpu" {
		t.Errorf("platform = %q, want cpu", workers[0].Hardware.Platform)
	}
}

func TestHub_join_rejected_bad_token(t *testing.T) {
	hub := server.NewHub(testToken)
	conn := dialHub(t, hub)

	sendMsg(t, conn, wire.MsgJoin, wire.JoinPayload{Token: "wrong-token", Version: "test"})
	ackEnv := readMsg(t, conn)
	var ack wire.JoinAckPayload
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ack.Accepted {
		t.Error("expected rejection, got accepted=true")
	}

	time.Sleep(20 * time.Millisecond)
	if n := len(hub.Workers()); n != 0 {
		t.Errorf("hub has %d workers after bad-token join, want 0", n)
	}
}

func TestHub_heartbeat_updates_last_seen(t *testing.T) {
	hub := server.NewHub(testToken)
	conn := dialHub(t, hub)

	ack := joinHub(t, conn, "")
	time.Sleep(20 * time.Millisecond)

	before := hub.Workers()[0].LastSeen
	time.Sleep(5 * time.Millisecond)

	sendMsg(t, conn, wire.MsgHeartbeat, wire.HeartbeatPayload{WorkerID: ack.WorkerID})
	hbAck := readMsg(t, conn)
	if hbAck.Type != wire.MsgHeartbeatAck {
		t.Errorf("got %q want heartbeat_ack", hbAck.Type)
	}

	time.Sleep(20 * time.Millisecond)
	after := hub.Workers()[0].LastSeen
	if !after.After(before) {
		t.Error("LastSeen did not advance after heartbeat")
	}
}

func TestHub_disconnect_removes_worker(t *testing.T) {
	hub := server.NewHub(testToken)
	conn := dialHub(t, hub)

	joinHub(t, conn, "")
	time.Sleep(20 * time.Millisecond)

	if n := len(hub.Workers()); n != 1 {
		t.Fatalf("expected 1 worker before disconnect, got %d", n)
	}

	_ = conn.Close()

	// Allow the handler goroutine to detect the disconnect and clean up.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(hub.Workers()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("worker still present 500ms after disconnect")
}

func TestHub_multiple_workers(t *testing.T) {
	hub := server.NewHub(testToken)

	conn1 := dialHub(t, hub)
	conn2 := dialHub(t, hub)

	joinHub(t, conn1, "worker-1")
	joinHub(t, conn2, "worker-2")
	time.Sleep(20 * time.Millisecond)

	if n := len(hub.Workers()); n != 2 {
		t.Fatalf("expected 2 workers, got %d", n)
	}
}
