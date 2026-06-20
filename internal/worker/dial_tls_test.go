package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/tlsx"
)

// wssURL converts an httptest TLS server URL (https://…) to a wss:// hub URL.
func wssURL(ts *httptest.Server) string {
	return "wss" + strings.TrimPrefix(ts.URL, "https")
}

// TestDial_wss_pinned_joins is the G14 wss:// case at the integration level: a
// worker joins a TLS server over wss://, accepting the connection because the
// server's self-signed certificate matches the pin (default chain/hostname
// validation is bypassed). Proves the pinning dial path connects end to end.
func TestDial_wss_pinned_joins(t *testing.T) {
	hub := server.NewHub(dialTestToken, nil)
	ts := httptest.NewTLSServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()

	pin := tlsx.Pin(ts.Certificate())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Dial(ctx, DialConfig{ServerURL: wssURL(ts), Token: dialTestToken, Name: "tls-joiner", TLSPin: pin})
	}()

	waitFor(t, func() bool { return len(hub.Workers()) == 1 }, 2*time.Second, "worker to join over wss://")

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

// TestDial_wss_wrong_pin_never_joins asserts the pin is enforced: a worker given
// the wrong pin cannot complete the TLS handshake, so it never appears in the
// hub's inventory no matter how many times it retries.
func TestDial_wss_wrong_pin_never_joins(t *testing.T) {
	origInitial, origMax := reconnectInitial, reconnectMax
	reconnectInitial, reconnectMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reconnectInitial, reconnectMax = origInitial, origMax })

	hub := server.NewHub(dialTestToken, nil)
	ts := httptest.NewTLSServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()

	// A valid-format pin that does not match the server's actual certificate.
	wrongPin, err := tlsx.NormalizePin(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Dial(ctx, DialConfig{ServerURL: wssURL(ts), Token: dialTestToken, Name: "wrong-pin", TLSPin: wrongPin})
	}()

	// Give the worker time to retry several times; it must never join.
	time.Sleep(300 * time.Millisecond)
	if n := len(hub.Workers()); n != 0 {
		t.Errorf("worker with wrong pin joined (%d workers); the pin was not enforced", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after ctx cancel")
	}
}
