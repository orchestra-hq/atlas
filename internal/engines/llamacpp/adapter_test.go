package llamacpp

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
// tests cover only the llama.cpp-specific endpoints: token counting via
// /apply-template + /tokenize, and the context window via /props.

// tokenizeServer fakes the /apply-template, /tokenize, and /props endpoints.
// applyCapture, if non-nil, receives the decoded /apply-template body.
func tokenizeServer(t *testing.T, prompt string, tokens int, nCtx int, applyCapture *applyTemplateRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/apply-template":
			if applyCapture != nil {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, applyCapture)
			}
			_ = json.NewEncoder(w).Encode(applyTemplateResponse{Prompt: prompt})
		case "/tokenize":
			ids := make([]int, tokens)
			_ = json.NewEncoder(w).Encode(tokenizeResponse{Tokens: ids})
		case "/props":
			var props propsResponse
			props.DefaultGenerationSettings.NCtx = nCtx
			_ = json.NewEncoder(w).Encode(props)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCountTokens(t *testing.T) {
	var apply applyTemplateRequest
	srv := tokenizeServer(t, "rendered prompt", 7, 0, &apply)
	defer srv.Close()

	a := New(srv.URL, "m", true, srv.Client())
	n, err := a.CountTokens(context.Background(), core.Request{
		Model:    "m",
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
	if n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
	// The rendered prompt must reflect the same system/tools/thinking a real
	// generation would use, or the count won't match usage.input_tokens.
	if len(apply.Messages) != 2 || apply.Messages[0].Role != "system" {
		t.Errorf("apply messages = %+v", apply.Messages)
	}
	if len(apply.Tools) != 1 || apply.Tools[0].Function.Name != "get_weather" {
		t.Errorf("apply tools = %+v", apply.Tools)
	}
	if apply.ChatTemplateKwargs["enable_thinking"] != true {
		t.Errorf("enable_thinking = %v, want true", apply.ChatTemplateKwargs["enable_thinking"])
	}
}

func TestContextWindow(t *testing.T) {
	srv := tokenizeServer(t, "", 0, 32768, nil)
	defer srv.Close()

	a := New(srv.URL, "m", true, srv.Client())
	n, err := a.ContextWindow(context.Background())
	if err != nil {
		t.Fatalf("ContextWindow: %v", err)
	}
	if n != 32768 {
		t.Errorf("context window = %d, want 32768", n)
	}
}

func TestCountTokensEngineErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
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
