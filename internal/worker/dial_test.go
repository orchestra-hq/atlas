package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/wire"
)

const dialTestToken = "dial-test-token"

// TestDial_joins_real_hub exercises the worker's Dial against the real server
// hub end-to-end: it joins, appears in the hub's inventory, and Dial returns
// cleanly when its context is cancelled.
func TestDial_joins_real_hub(t *testing.T) {
	hub := server.NewHub(dialTestToken)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Dial(ctx, DialConfig{ServerURL: url, Token: dialTestToken, Name: "joiner"})
	}()

	waitFor(t, func() bool { return len(hub.Workers()) == 1 }, 2*time.Second, "worker to join")
	if name := hub.Workers()[0].Name; name != "joiner" {
		t.Errorf("worker name = %q, want joiner", name)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Dial returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after ctx cancel")
	}
}

// TestDial_reconnects_after_drop exercises the phase 1 reconnect criterion: the
// server drops the first connection right after the join, and the worker dials
// back in on its own. The handler closes the connection from inside the handler
// (net/http does not close hijacked WebSocket connections externally).
func TestDial_reconnects_after_drop(t *testing.T) {
	origInitial, origMax := reconnectInitial, reconnectMax
	reconnectInitial, reconnectMax = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { reconnectInitial, reconnectMax = origInitial, origMax })

	var connCount atomic.Int64
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connCount.Add(1)
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()

		if _, _, err := c.ReadMessage(); err != nil { // consume join
			return
		}
		ackMsg, _ := wire.Encode(wire.MsgJoinAck, "", wire.JoinAckPayload{Accepted: true, WorkerID: "w_test"})
		data, _ := json.Marshal(ackMsg)
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
		if n == 1 {
			return // simulate a drop: the deferred Close drops the connection
		}
		for { // later connections: stay alive until the client goes away
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Dial(ctx, DialConfig{ServerURL: url, Token: "tok", Name: "reconnector"})
	}()

	waitFor(t, func() bool { return connCount.Load() >= 2 }, 2*time.Second, "worker to reconnect after drop")
}

// TestDial_rejected_bad_token_keeps_retrying confirms a worker whose token is
// rejected does not join, and the dial loop keeps trying rather than exiting.
func TestDial_rejected_bad_token_keeps_retrying(t *testing.T) {
	origInitial, origMax := reconnectInitial, reconnectMax
	reconnectInitial, reconnectMax = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { reconnectInitial, reconnectMax = origInitial, origMax })

	hub := server.NewHub("correct-token")
	var connCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		hub.HandleConnect(w, r)
	}))
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Dial(ctx, DialConfig{ServerURL: url, Token: "wrong-token", Name: "rejected"})
	}()

	waitFor(t, func() bool { return connCount.Load() >= 2 }, 2*time.Second, "repeated join attempts")
	if n := len(hub.Workers()); n != 0 {
		t.Errorf("rejected worker registered: %d workers", n)
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
