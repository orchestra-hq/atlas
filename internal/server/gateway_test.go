package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

const (
	testKey   = "test-key"
	testModel = "test-model"
)

// echoExecutor replies with a fixed text, echoing the request so tests can
// assert the gateway stripped stop sequences before dispatch.
type echoExecutor struct {
	reply    string
	gotReq   core.Request
	err      error
	outToken int
}

func (e *echoExecutor) Execute(_ context.Context, req core.Request) (core.Response, error) {
	e.gotReq = req
	if e.err != nil {
		return core.Response{}, e.err
	}
	out := e.outToken
	if out == 0 {
		out = 5
	}
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock(e.reply)},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 7, OutputTokens: out},
	}, nil
}

func newTestServer(exec Executor) *httptest.Server {
	g := NewGateway(testKey, map[string]Executor{testModel: exec})
	return httptest.NewServer(g.Handler())
}

func post(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

func TestMessagesHappyPath(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "hello there"})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != testModel {
		t.Errorf("envelope = %v", body)
	}
	if body["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	first := content[0].(map[string]any)
	if first["text"] != "hello there" {
		t.Errorf("text = %v", first["text"])
	}
}

func TestStopSequenceAppliedByGateway(t *testing.T) {
	exec := &echoExecutor{reply: "one two three four"}
	srv := newTestServer(exec)
	defer srv.Close()

	resp, body := post(t, srv, testKey,
		`{"model":"test-model","max_tokens":16,"stop_sequences":["three"],"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body["stop_reason"] != "stop_sequence" || body["stop_sequence"] != "three" {
		t.Errorf("stop = %v / %v", body["stop_reason"], body["stop_sequence"])
	}
	content := body["content"].([]any)
	if got := content[0].(map[string]any)["text"]; got != "one two " {
		t.Errorf("text = %q", got)
	}
	// The gateway must not leak stop sequences to the engine.
	if exec.gotReq.StopSequences != nil {
		t.Errorf("executor saw stop sequences: %v", exec.gotReq.StopSequences)
	}
}

func TestAuthErrors(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	body := `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	for _, key := range []string{"", "wrong-key"} {
		resp, parsed := post(t, srv, key, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
		if errType(parsed) != "authentication_error" {
			t.Errorf("key %q: type = %v", key, errType(parsed))
		}
	}
}

func TestBearerAuthAccepted(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownModel404(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"nope","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound || errType(body) != "not_found_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

func TestStreamRejected(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, body := post(t, srv, testKey,
		`{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest || errType(body) != "invalid_request_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

func TestValidationErrors(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	tests := []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing model", `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"missing max_tokens", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"test-model","max_tokens":16,"messages":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := post(t, srv, testKey, tc.body)
			if resp.StatusCode != http.StatusBadRequest || errType(body) != "invalid_request_error" {
				t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
			}
		})
	}
}

func TestEngineErrorIsApiError(t *testing.T) {
	srv := newTestServer(&echoExecutor{err: io.ErrUnexpectedEOF})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusInternalServerError || errType(body) != "api_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func errType(body map[string]any) any {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return nil
	}
	return e["type"]
}
