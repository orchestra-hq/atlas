package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/orchestra-hq/atlas/internal/core"
)

// EmbeddingsRequest is the OpenAI POST /v1/embeddings request (M3 phase 2a,
// ADR-0012). Input accepts a single string or an array of strings — the two shapes
// the OpenAI SDK sends — normalized to a slice by StringOrStrings. Token-array
// inputs ([]int) are not supported; such a body fails decode and is rejected with a
// clean 400. EncodingFormat is honored: "base64" returns little-endian IEEE 754
// float32 bytes base64-encoded as a JSON string; "float" (the default when empty)
// returns a JSON float array.
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
// results with inputs. Embedding is a pre-encoded JSON value: a float array when
// encoding_format is "float" (the default), a JSON string when "base64".
type EmbeddingDatum struct {
	Object    string          `json:"object"` // always "embedding"
	Index     int             `json:"index"`
	Embedding json.RawMessage `json:"embedding"`
}

// EmbeddingsUsage is the embeddings token accounting (no completion tokens).
type EmbeddingsUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// FromCoreEmbeddings shapes a core embeddings result into the OpenAI response,
// echoing the model name the client addressed. encodingFormat controls how each
// vector is serialized: "base64" produces a little-endian IEEE 754 float32 byte
// array encoded as a base64 JSON string (the OpenAI Python SDK default); anything
// else ("float" or empty) produces a plain JSON float array.
func FromCoreEmbeddings(model, encodingFormat string, resp core.EmbedResponse) EmbeddingsResponse {
	data := make([]EmbeddingDatum, len(resp.Embeddings))
	for i, v := range resp.Embeddings {
		var raw json.RawMessage
		if encodingFormat == "base64" {
			raw, _ = json.Marshal(float32sToBase64(v))
		} else {
			raw, _ = json.Marshal(v)
		}
		data[i] = EmbeddingDatum{Object: "embedding", Index: i, Embedding: raw}
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

// float32sToBase64 encodes a float32 slice as a standard base64 string of
// little-endian IEEE 754 bytes — the shape the OpenAI Python SDK expects when it
// sends encoding_format: "base64".
func float32sToBase64(vecs []float32) string {
	buf := make([]byte, len(vecs)*4)
	for i, v := range vecs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
