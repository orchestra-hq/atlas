// Package mlx adapts an mlx-lm server (Apple-Silicon inference via Metal) to
// Atlas's engine interface. Like the vLLM adapter (build-time decision 1,
// docs/internal/m0-build-plan.md), it speaks the OpenAI-compatible
// /v1/chat/completions endpoint via the shared internal/engines/openaichat
// translation, so one conformance result holds across engines. mlx_lm.server
// exposes no tokenize or model-metadata endpoint, so the two engine-specific
// methods are sourced differently than vLLM's (see CountTokens, ContextWindow).
package mlx

import (
	"context"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/openaichat"
)

// Adapter executes core requests against a running mlx_lm.server. contextWindow is
// the model's window taken from the catalog (mlx_lm.server's /v1/models reports no
// length), used to answer ContextWindow without an engine round-trip.
type Adapter struct {
	*openaichat.Client
	contextWindow int
}

// New builds an adapter targeting an mlx_lm.server at baseURL (e.g.
// http://127.0.0.1:8080). model must be the id the server loaded — mlx_lm.server
// has no --served-model-name and loads exactly the id a request names, so the
// caller passes the model's Hugging Face repo id, not Atlas's logical name.
// contextWindow is the catalog's window for the model (0 = unknown, assertion
// skipped). reasoning is the model's catalog reasoning capability, which gates
// the thinking kwarg (M2 phase 4b).
func New(baseURL, model string, contextWindow int, reasoning bool, client *http.Client) *Adapter {
	return &Adapter{
		Client:        openaichat.NewClient("mlx", baseURL, model, reasoning, client),
		contextWindow: contextWindow,
	}
}

// CountTokens returns the prompt's token count using the engine's real tokenizer
// and chat template (ADR build-time decision 2: never reimplement tokenization).
// mlx_lm.server has no /tokenize endpoint, but it returns usage.prompt_tokens on a
// completion, so a one-token probe (max_tokens 1) yields the exact input-token
// count an identical Execute would report — the chat template and tokenizer are the
// same, and the single generated token does not affect the prompt count. The probe
// costs one prefill, so callers use it for the explicit count-tokens surface, not
// as a per-request pre-check on the hot path.
func (a *Adapter) CountTokens(ctx context.Context, req core.Request) (int, error) {
	probe := req
	probe.MaxTokens = 1
	resp, err := a.Execute(ctx, probe)
	if err != nil {
		return 0, err
	}
	return resp.Usage.InputTokens, nil
}

// ContextWindow returns the model's context window in tokens. mlx_lm.server's
// /v1/models exposes no length, so this is the catalog-provided window the adapter
// was built with (0 when unknown, which makes the gateway skip the fit assertion).
func (a *Adapter) ContextWindow(_ context.Context) (int, error) {
	return a.contextWindow, nil
}
