package sglang

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// Generation (Execute/ExecuteStream) and core⇄OpenAI translation are tested in
// internal/engines/openaichat, the shared layer this adapter embeds. These tests
// cover only the SGLang-specific behavior: the context window via /v1/models
// max_model_len (same shape vLLM serves), and CountTokens via a one-token
// completion probe (SGLang has no OpenAI /tokenize endpoint).

func TestCountTokensViaProbe(t *testing.T) {
	var got struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":27,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	a := New(srv.URL, "served-model", true, srv.Client())
	n, err := a.CountTokens(context.Background(), core.Request{
		Model:     "served-model",
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 27 {
		t.Errorf("count = %d, want 27 (usage.prompt_tokens)", n)
	}
	if got.MaxTokens != 1 {
		t.Errorf("probe max_tokens = %d, want 1", got.MaxTokens)
	}
	// SGLang answers to the logical name via --served-model-name, so the adapter
	// echoes it (unlike MLX, which echoes the repo id).
	if got.Model != "served-model" {
		t.Errorf("probe model = %q, want served-model", got.Model)
	}
}

func TestContextWindowMatchesServedModel(t *testing.T) {
	srv := modelsServer(t, `{"object":"list","data":[
		{"id":"other","object":"model","max_model_len":2048},
		{"id":"served-model","object":"model","max_model_len":40960}
	]}`)
	defer srv.Close()

	a := New(srv.URL, "served-model", true, srv.Client())
	n, err := a.ContextWindow(context.Background())
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if n != 40960 {
		t.Errorf("context window = %d, want 40960 (served model)", n)
	}
}

func TestContextWindowFallsBackToFirst(t *testing.T) {
	srv := modelsServer(t, `{"object":"list","data":[
		{"id":"only-model","object":"model","max_model_len":8192}
	]}`)
	defer srv.Close()

	a := New(srv.URL, "name-the-server-does-not-report", true, srv.Client())
	n, err := a.ContextWindow(context.Background())
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if n != 8192 {
		t.Errorf("context window = %d, want 8192 (first entry)", n)
	}
}

func TestContextWindowNoModels(t *testing.T) {
	srv := modelsServer(t, `{"object":"list","data":[]}`)
	defer srv.Close()

	a := New(srv.URL, "m", true, srv.Client())
	if _, err := a.ContextWindow(context.Background()); err == nil {
		t.Fatal("expected error when /v1/models returns no models")
	}
}

func modelsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
}
