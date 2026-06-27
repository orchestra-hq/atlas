// Package sglang adapts an SGLang server to Atlas's engine interface. Like the
// vLLM adapter (build-time decision 1, docs/internal/m0-build-plan.md), it speaks the
// OpenAI-compatible /v1/chat/completions endpoint via the shared
// internal/engines/openaichat translation, so one conformance result holds across
// engines. The two engine-specific methods differ from vLLM's: SGLang's /v1/models
// reports the context window as max_model_len (same as vLLM), but it has no
// OpenAI /tokenize endpoint, so CountTokens uses a one-token completion probe.
package sglang

import (
	"context"
	"fmt"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/openaichat"
)

// Adapter executes core requests against a running SGLang server.
type Adapter struct {
	*openaichat.Client
}

// New builds an adapter targeting an SGLang server at baseURL (e.g.
// http://127.0.0.1:30000). model is the name echoed in the OpenAI payload and
// reported by /v1/models; SGLang accepts --served-model-name, so it answers to
// Atlas's logical name (like vLLM) and the adapter echoes that. reasoning is the
// model's catalog reasoning capability, which gates the thinking kwarg (M2 phase
// 4b).
func New(baseURL, model string, reasoning bool, client *http.Client) *Adapter {
	return &Adapter{Client: openaichat.NewClient("sglang", baseURL, model, reasoning, client)}
}

// modelsResponse is the subset of GET /v1/models Atlas reads. SGLang reports each
// served model's context window as max_model_len, the same field vLLM uses.
type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// CountTokens returns the prompt's token count using the engine's real tokenizer
// and chat template (ADR build-time decision 2: never reimplement tokenization).
// SGLang exposes no OpenAI /tokenize endpoint, but it returns usage.prompt_tokens
// on a completion, so a one-token probe (max_tokens 1) yields the exact
// input-token count an identical Execute would report — the single generated token
// does not affect the prompt count. The probe costs one prefill, so callers use it
// for the explicit count-tokens surface, not as a per-request pre-check.
func (a *Adapter) CountTokens(ctx context.Context, req core.Request) (int, error) {
	probe := req
	probe.MaxTokens = 1
	resp, err := a.Execute(ctx, probe)
	if err != nil {
		return 0, err
	}
	return resp.Usage.InputTokens, nil
}

// ContextWindow returns the engine's context window (tokens), read as
// max_model_len from GET /v1/models — the same shape vLLM serves. The gateway uses
// it to reject oversized requests pre-dispatch and to report each model's window.
func (a *Adapter) ContextWindow(ctx context.Context) (int, error) {
	var models modelsResponse
	if err := a.GetJSON(ctx, "/v1/models", &models); err != nil {
		return 0, err
	}
	if len(models.Data) == 0 {
		return 0, fmt.Errorf("sglang: /v1/models returned no models")
	}
	// Prefer the served model by id; fall back to the first entry (an id that
	// differs from the addressed name).
	for _, m := range models.Data {
		if m.ID == a.Model() {
			return m.MaxModelLen, nil
		}
	}
	return models.Data[0].MaxModelLen, nil
}
