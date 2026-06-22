package openai

import "github.com/orchestra-hq/atlas/internal/core"

// EmbeddingsRequest is the OpenAI POST /v1/embeddings request (M3 phase 2a,
// ADR-0012). Input accepts a single string or an array of strings — the two shapes
// the OpenAI SDK sends — normalized to a slice by StringOrStrings. Token-array
// inputs ([]int) are not supported; such a body fails decode and is rejected with a
// clean 400. EncodingFormat is accepted and ignored: Atlas always returns float
// vectors (the engines' native shape), which is the OpenAI `float` format.
type EmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          StringOrStrings `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

// ToCore maps the request to the engine-independent core form.
func (r EmbeddingsRequest) ToCore() core.EmbedRequest {
	return core.EmbedRequest{Model: r.Model, Input: r.Input.Values}
}

// EmbeddingsResponse is the OpenAI /v1/embeddings response: a list of per-input
// embedding objects plus token usage.
type EmbeddingsResponse struct {
	Object string           `json:"object"` // always "list"
	Data   []EmbeddingDatum `json:"data"`
	Model  string           `json:"model"`
	Usage  EmbeddingsUsage  `json:"usage"`
}

// EmbeddingDatum is one input's vector, carrying its index so a client can realign
// results with inputs.
type EmbeddingDatum struct {
	Object    string    `json:"object"` // always "embedding"
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// EmbeddingsUsage is the embeddings token accounting (no completion tokens).
type EmbeddingsUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// FromCoreEmbeddings shapes a core embeddings result into the OpenAI response,
// echoing the model name the client addressed.
func FromCoreEmbeddings(model string, resp core.EmbedResponse) EmbeddingsResponse {
	data := make([]EmbeddingDatum, len(resp.Embeddings))
	for i, v := range resp.Embeddings {
		data[i] = EmbeddingDatum{Object: "embedding", Index: i, Embedding: v}
	}
	return EmbeddingsResponse{
		Object: "list",
		Data:   data,
		Model:  model,
		Usage: EmbeddingsUsage{
			PromptTokens: resp.Usage.InputTokens,
			TotalTokens:  resp.Usage.InputTokens,
		},
	}
}
