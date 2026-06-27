// Package vllm adapts a vLLM OpenAI-compatible server to Atlas's engine
// interface. Per build-time decision 1 in docs/internal/m0-build-plan.md, the adapter
// speaks vLLM's /v1/chat/completions endpoint (via the shared
// internal/engines/openaichat translation); the gateway produces all Anthropic
// semantics, so one conformance result holds across engines. Only token
// counting and the context-window query — which use vLLM-specific endpoints —
// live here.
package vllm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/openaichat"
)

// Adapter executes core requests against a running vLLM server.
type Adapter struct {
	*openaichat.Client
}

// New builds an adapter targeting a vLLM server at baseURL (e.g.
// http://127.0.0.1:8000). model is the name echoed in the OpenAI payload and
// addressed on vLLM's per-model endpoints; vLLM serves whatever weights it was
// launched with.
func New(baseURL, model string, reasoning bool, client *http.Client) *Adapter {
	return &Adapter{Client: openaichat.NewClient("vllm", baseURL, model, reasoning, client)}
}

// tokenizeRequest is vLLM's POST /tokenize body in chat form: it applies the
// model's chat template server-side (so we send the same messages, tools, and
// thinking kwarg a generation would) and returns the resulting token count.
// add_generation_prompt mirrors generation, so the count matches an identical
// request's usage.input_tokens.
type tokenizeRequest struct {
	Model               string               `json:"model"`
	Messages            []openaichat.Message `json:"messages"`
	Tools               []openaichat.Tool    `json:"tools,omitempty"`
	ChatTemplateKwargs  map[string]any       `json:"chat_template_kwargs,omitempty"`
	AddGenerationPrompt bool                 `json:"add_generation_prompt"`
}

type tokenizeResponse struct {
	Count int `json:"count"`
}

// modelsResponse is the subset of GET /v1/models Atlas reads. vLLM reports each
// served model's context window as max_model_len.
type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// CountTokens returns the prompt's token count using vLLM's real tokenizer
// (ADR build-time decision 2: never reimplement tokenization). vLLM's /tokenize
// applies the chat template itself, so a single call — with the same messages,
// tools, and thinking kwarg generation would use — yields a count equal to the
// usage.input_tokens an identical Execute would report.
func (a *Adapter) CountTokens(ctx context.Context, req core.Request) (int, error) {
	var tok tokenizeResponse
	if err := a.PostJSON(ctx, "/tokenize", tokenizeRequest{
		Model:               a.Model(),
		Messages:            openaichat.Messages(req),
		Tools:               openaichat.Tools(req.Tools),
		ChatTemplateKwargs:  a.ThinkingKwargs(req),
		AddGenerationPrompt: true,
	}, &tok); err != nil {
		return 0, err
	}
	return tok.Count, nil
}

// ContextWindow returns the engine's context window (tokens), read as
// max_model_len from GET /v1/models. The gateway uses it to reject oversized
// requests pre-dispatch and to report each model's window via /v1/models
// (docs/internal/m0-acceptance.md).
func (a *Adapter) ContextWindow(ctx context.Context) (int, error) {
	var models modelsResponse
	if err := a.GetJSON(ctx, "/v1/models", &models); err != nil {
		return 0, err
	}
	if len(models.Data) == 0 {
		return 0, fmt.Errorf("vllm: /v1/models returned no models")
	}
	// Prefer the served model by id; fall back to the first entry (single-model
	// servers, or an id that differs from the addressed name).
	for _, m := range models.Data {
		if m.ID == a.Model() {
			return m.MaxModelLen, nil
		}
	}
	return models.Data[0].MaxModelLen, nil
}
