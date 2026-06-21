package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the test goroutine to read while the
// request goroutine writes the log line (which happens after the handler returns).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitForLog polls the buffer until it contains substr or the deadline passes
// (the request log is written after the handler returns, racing the client read).
func waitForLog(t *testing.T, buf *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s := buf.String(); strings.Contains(s, substr) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q:\n%s", substr, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRequestLogIncludesWorkerAndKeyForBillable is the log-polish case (M2 phase
// 1b): a billable inference request's log line carries the serving worker and
// calling key, so it correlates with the per-worker/key metrics and the ledger.
func TestRequestLogIncludesWorkerAndKeyForBillable(t *testing.T) {
	buf := &syncBuffer{}
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.SetLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	g.RegisterInstance("w_conn", "gpu-box", Model{Name: testModel, Exec: &echoExecutor{reply: "hi", outToken: 4}, ContextWindow: 4096})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	if resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	line := waitForLog(t, buf, "worker=gpu-box")
	if !strings.Contains(line, "key_id=test") {
		t.Errorf("billable request log missing key_id:\n%s", line)
	}
}

// TestRequestLogOmitsWorkerForNonBillable asserts the non-inference paths
// (count_tokens here) do not log worker/key fields they don't have.
func TestRequestLogOmitsWorkerForNonBillable(t *testing.T) {
	buf := &syncBuffer{}
	g := NewGateway(staticAuth(testKey),
		[]Model{{Name: testModel, Exec: &countingExecutor{count: 9}, ContextWindow: 4096}}, nil)
	g.SetLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", testKey)
	req.Header.Set("content-type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	line := waitForLog(t, buf, "path=/v1/messages/count_tokens")
	if strings.Contains(line, "key_id=") || strings.Contains(line, "worker=") {
		t.Errorf("non-billable request log should omit worker/key fields:\n%s", line)
	}
}
