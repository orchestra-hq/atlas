package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzAlwaysOK(t *testing.T) {
	// Liveness does not depend on a model being loaded.
	g := NewGateway(testKey, nil, nil)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestReadyzReflectsServableModels(t *testing.T) {
	// No models: not ready.
	empty := httptest.NewServer(NewGateway(testKey, nil, nil).Handler())
	t.Cleanup(empty.Close)
	if got := getStatus(t, empty, "/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("empty /readyz = %d, want 503", got)
	}

	// A registered model: ready.
	ready := newTestServer(&echoExecutor{reply: "hi"})
	t.Cleanup(ready.Close)
	if got := getStatus(t, ready, "/readyz"); got != http.StatusOK {
		t.Errorf("ready /readyz = %d, want 200", got)
	}
}

// TestRequestLogIncludesTokenCounts is the G10 substrate: each request emits a
// structured log line carrying its input and output token counts.
func TestRequestLogIncludesTokenCounts(t *testing.T) {
	var buf bytes.Buffer
	g := NewGateway(testKey, []Model{{Name: testModel, Exec: &echoExecutor{reply: "hello", outToken: 11}}}, nil)
	g.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	line := buf.String()
	for _, want := range []string{
		"path=/v1/messages",
		"status=200",
		"model=test-model",
		"input_tokens=7",
		"output_tokens=11",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q\ngot: %s", want, line)
		}
	}
}

// TestProbesAreNotLogged keeps liveness/readiness polling out of the request
// log so it does not drown the per-request token accounting.
func TestProbesAreNotLogged(t *testing.T) {
	var buf bytes.Buffer
	g := NewGateway(testKey, []Model{{Name: testModel, Exec: &echoExecutor{reply: "hi"}}}, nil)
	g.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	_ = getStatus(t, srv, "/healthz")
	_ = getStatus(t, srv, "/readyz")
	if buf.Len() != 0 {
		t.Errorf("probes were logged: %s", buf.String())
	}
}

func getStatus(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}
