package vllm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// Generation (Execute/ExecuteStream) and core⇄OpenAI translation are tested in
// internal/engines/openaichat, the shared layer this adapter embeds. These
// tests cover only the vLLM-specific endpoints: token counting via /tokenize
// (chat form) and the context window via /v1/models max_model_len.

func TestCountTokens(t *testing.T) {
	var got tokenizeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenizeResponse{Count: 13})
	}))
	defer srv.Close()

	a := New(srv.URL, "served-model", true, srv.Client())
	n, err := a.CountTokens(context.Background(), core.Request{
		Model:    "served-model",
		System:   "be terse",
		Thinking: &core.ThinkingConfig{Enabled: true},
		Tools: []core.Tool{{
			Name:        "get_weather",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 13 {
		t.Errorf("count = %d, want 13", n)
	}
	// The tokenize request must reflect the same model/system/tools/thinking and
	// add_generation_prompt a real generation would, or the count won't match
	// usage.input_tokens.
	if got.Model != "served-model" {
		t.Errorf("model = %q", got.Model)
	}
	if !got.AddGenerationPrompt {
		t.Error("add_generation_prompt must be true")
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if got.ChatTemplateKwargs["enable_thinking"] != true {
		t.Errorf("enable_thinking = %v, want true", got.ChatTemplateKwargs["enable_thinking"])
	}
}

func TestContextWindowMatchesServedModel(t *testing.T) {
	srv := modelsServer(t, `{"object":"list","data":[
		{"id":"other","object":"model","max_model_len":2048},
		{"id":"served-model","object":"model","max_model_len":32768}
	]}`)
	defer srv.Close()

	a := New(srv.URL, "served-model", true, srv.Client())
	n, err := a.ContextWindow(context.Background())
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if n != 32768 {
		t.Errorf("context window = %d, want 32768 (served model)", n)
	}
}

func TestContextWindowFallsBackToFirst(t *testing.T) {
	srv := modelsServer(t, `{"object":"list","data":[
		{"id":"only-model","object":"model","max_model_len":4096}
	]}`)
	defer srv.Close()

	a := New(srv.URL, "name-the-server-does-not-report", true, srv.Client())
	n, err := a.ContextWindow(context.Background())
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if n != 4096 {
		t.Errorf("context window = %d, want 4096 (first entry)", n)
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

func TestCountTokensEngineErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"loading"}`)
	}))
	defer srv.Close()

	a := New(srv.URL, "m", true, srv.Client())
	_, err := a.CountTokens(context.Background(), core.Request{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if !errors.Is(err, core.ErrEngineUnavailable) {
		t.Errorf("err = %v, want wrapped ErrEngineUnavailable", err)
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
