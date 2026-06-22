package openaichat

import (
	"context"
	"fmt"
	"sort"

	"github.com/orchestra-hq/atlas/internal/core"
)

// embedRequest is the OpenAI /v1/embeddings request body. encoding_format is
// pinned to "float" so the engine returns plain JSON number arrays rather than
// base64, which keeps decoding uniform across engines (vLLM/SGLang default to
// float already; pinning it makes the wire shape explicit).
type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// embedResponse is the OpenAI /v1/embeddings response. Engines do not guarantee
// data is returned in input order, so each datum carries its index and Embed
// sorts on it before returning.
type embedResponse struct {
	Data  []embedDatum `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type embedDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embed calls the engine's OpenAI-compatible /v1/embeddings endpoint and maps the
// result back to core (M3 phase 2a, ADR-0012). Atlas computes nothing — it forwards
// the inputs to the engine and shapes the response — so this is the embedding
// sibling of Execute, shared by every adapter that embeds a Client. An engine that
// was not launched in embedding mode answers non-200, which PostJSON surfaces as
// core.ErrEngineUnavailable (the gateway maps it to a retryable 529).
func (c *Client) Embed(ctx context.Context, req core.EmbedRequest) (core.EmbedResponse, error) {
	if len(req.Input) == 0 {
		return core.EmbedResponse{}, nil
	}
	var out embedResponse
	if err := c.PostJSON(ctx, "/v1/embeddings", embedRequest{
		Model:          c.model,
		Input:          req.Input,
		EncodingFormat: "float",
	}, &out); err != nil {
		return core.EmbedResponse{}, err
	}
	if len(out.Data) != len(req.Input) {
		return core.EmbedResponse{}, fmt.Errorf("%s: embeddings: engine returned %d vectors for %d inputs", c.name, len(out.Data), len(req.Input))
	}
	// Restore input order: the spec allows the engine to return data in any order,
	// keyed by index.
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return core.EmbedResponse{
		Embeddings: vecs,
		Usage:      core.Usage{InputTokens: out.Usage.PromptTokens},
	}, nil
}
