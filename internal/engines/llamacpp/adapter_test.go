package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// fakeServer stands in for llama-server's /v1/chat/completions endpoint.
func fakeServer(t *testing.T, status int, respBody string, capture *chatRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

func temp(f float64) *float64 { return &f }

func TestExecuteTranslatesAndMaps(t *testing.T) {
	var got chatRequest
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":2}
	}`, &got)
	defer srv.Close()

	a := New(srv.URL, "served-model", srv.Client())
	resp, err := a.Execute(context.Background(), core.Request{
		Model:       "served-model",
		System:      "be terse",
		MaxTokens:   32,
		Temperature: temp(0),
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Response mapping.
	if resp.Text() != "hello world" {
		t.Errorf("text = %q", resp.Text())
	}
	if resp.StopReason != core.StopEndTurn {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Request translation: system message prepended, sampling forwarded.
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "be terse" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "hi" {
		t.Errorf("user message = %+v", got.Messages[1])
	}
	if got.MaxTokens != 32 || got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("max_tokens/temperature = %d / %v", got.MaxTokens, got.Temperature)
	}
	if got.Stream {
		t.Error("stream must be false")
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]core.StopReason{
		"stop":   core.StopEndTurn,
		"length": core.StopMaxTokens,
		"":       core.StopEndTurn,
		"weird":  core.StopEndTurn,
	}
	for reason, want := range cases {
		if got := mapFinishReason(reason); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestExecuteEngineErrorStatus(t *testing.T) {
	srv := fakeServer(t, http.StatusServiceUnavailable, `{"error":"loading model"}`, nil)
	defer srv.Close()

	a := New(srv.URL, "m", srv.Client())
	_, err := a.Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestExecuteNoChoices(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, `{"choices":[],"usage":{}}`, nil)
	defer srv.Close()

	a := New(srv.URL, "m", srv.Client())
	_, err := a.Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error when no choices returned")
	}
}
