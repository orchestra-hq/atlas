// Package llamacpp adapts llama.cpp's bundled HTTP server (llama-server) to
// Atlas's engine interface. Per build-time decision 1 in
// docs/internal/m0-build-plan.md, the adapter speaks llama-server's OpenAI-compatible
// /v1/chat/completions endpoint (via the shared internal/engines/openaichat
// translation); the gateway produces all Anthropic semantics, so one
// conformance result holds across engines. Only token counting and the
// context-window query — which use llama-server-specific endpoints — live here.
package llamacpp

import (
	"context"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/openaichat"
)

// Adapter executes core requests against a running llama-server instance.
type Adapter struct {
	*openaichat.Client
}

// New builds an adapter targeting a llama-server at baseURL (e.g.
// http://127.0.0.1:8080). model is the name echoed in the OpenAI payload;
// llama-server serves whatever weights it was launched with regardless.
// reasoning is the model's catalog reasoning capability, which gates the
// thinking kwarg (M2 phase 4b).
func New(baseURL, model string, reasoning bool, client *http.Client) *Adapter {
	return &Adapter{Client: openaichat.NewClient("llamacpp", baseURL, model, reasoning, client)}
}

// applyTemplateRequest asks llama-server to render the chat template for the
// given messages without generating. The body mirrors a chat completion so the
// rendered prompt matches what generation would feed the model — tools and the
// thinking kwarg both change the template (and thus the token count).
type applyTemplateRequest struct {
	Messages           []openaichat.Message `json:"messages"`
	Tools              []openaichat.Tool    `json:"tools,omitempty"`
	ChatTemplateKwargs map[string]any       `json:"chat_template_kwargs,omitempty"`
}

type applyTemplateResponse struct {
	Prompt string `json:"prompt"`
}

type tokenizeRequest struct {
	Content string `json:"content"`
}

type tokenizeResponse struct {
	Tokens []int `json:"tokens"`
}

// propsResponse is the subset of GET /props Atlas reads. n_ctx lives under
// default_generation_settings and is the context window the server was launched
// with (the model's trained window unless capped with -c).
type propsResponse struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

// CountTokens returns the prompt's token count using the engine's real
// tokenizer (ADR build-time decision 2: never reimplement tokenization). It
// renders the chat template via /apply-template, then tokenizes the result via
// /tokenize — the same path generation takes, so the count equals the
// usage.input_tokens an identical Execute would report.
func (a *Adapter) CountTokens(ctx context.Context, req core.Request) (int, error) {
	var rendered applyTemplateResponse
	if err := a.PostJSON(ctx, "/apply-template", applyTemplateRequest{
		Messages:           openaichat.Messages(req),
		Tools:              openaichat.Tools(req.Tools),
		ChatTemplateKwargs: a.ThinkingKwargs(req),
	}, &rendered); err != nil {
		return 0, err
	}
	var tok tokenizeResponse
	if err := a.PostJSON(ctx, "/tokenize", tokenizeRequest{Content: rendered.Prompt}, &tok); err != nil {
		return 0, err
	}
	return len(tok.Tokens), nil
}

// ContextWindow returns the engine's context window (tokens), read from /props.
// The gateway uses it to reject oversized requests pre-dispatch and to report
// each model's window via /v1/models (docs/internal/m0-acceptance.md).
func (a *Adapter) ContextWindow(ctx context.Context) (int, error) {
	var props propsResponse
	if err := a.GetJSON(ctx, "/props", &props); err != nil {
		return 0, err
	}
	return props.DefaultGenerationSettings.NCtx, nil
}
