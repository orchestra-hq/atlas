package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/server"
)

// TestRenderTopRates asserts the live view shows placeholders on the first frame
// (no previous sample) and computes per-second rates from the delta on the next.
func TestRenderTopRates(t *testing.T) {
	t0 := time.Now()
	first := &topSample{at: t0, status: server.FleetStatus{
		Metrics: server.MetricsSnapshot{Requests: 0, Errors: 0, InputTokens: 0, OutputTokens: 0},
	}}
	second := &topSample{at: t0.Add(2 * time.Second), status: server.FleetStatus{
		Metrics: server.MetricsSnapshot{Requests: 10, Errors: 2, InFlight: 1, InputTokens: 100, OutputTokens: 50, QueueDepth: 3, Shed: 7},
	}}

	var firstFrame bytes.Buffer
	renderTop(&firstFrame, first, nil)
	if !strings.Contains(firstFrame.String(), "—") {
		t.Errorf("first frame should show rate placeholders:\n%s", firstFrame.String())
	}

	var nextFrame bytes.Buffer
	renderTop(&nextFrame, second, first)
	got := nextFrame.String()
	// 10 requests over 2s = 5.0/s; 100 input tokens = 50/s; 50 output = 25/s; 2 errors = 1.0/s.
	for _, want := range []string{"5.0/s", "50/s", "25/s", "1.0/s", "1 in flight", "3 queued", "7 shed"} {
		if !strings.Contains(got, want) {
			t.Errorf("next frame missing %q:\n%s", want, got)
		}
	}
}

// TestRunTopFailsFastOnFirstError asserts top returns the error immediately when
// the very first poll fails (a misconfigured target), rather than spinning.
func TestRunTopFailsFastOnFirstError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := newAdminClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("newAdminClient: %v", err)
	}
	cmd := testCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)

	done := make(chan error, 1)
	go func() { done <- runTop(cmd, client, time.Second) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("runTop returned nil on a failing first poll, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTop did not fail fast on the first poll error")
	}
}

// TestRunTopExitsOnContextCancel asserts a live session exits cleanly when its
// context is cancelled (Ctrl-C), after at least one successful frame.
func TestRunTopExitsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"workers":[],"deployments":[],"metrics":{"requests":1}}`))
	}))
	defer srv.Close()

	client, err := newAdminClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("newAdminClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmd()
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)

	done := make(chan error, 1)
	go func() { done <- runTop(cmd, client, time.Second) }()
	time.Sleep(50 * time.Millisecond) // let the first frame render
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runTop returned %v on context cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTop did not exit on context cancel")
	}
	if !strings.Contains(out.String(), "atlas top") {
		t.Errorf("expected at least one frame before cancel:\n%s", out.String())
	}
}
