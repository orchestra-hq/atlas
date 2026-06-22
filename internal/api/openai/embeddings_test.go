package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// TestEmbeddingsRequest_ToCore: both input shapes the OpenAI SDK sends — a single
// string and an array — normalize to a core input slice.
func TestEmbeddingsRequest_ToCore(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"array":  {`{"model":"m","input":["a","b"]}`, []string{"a", "b"}},
		"string": {`{"model":"m","input":"solo"}`, []string{"solo"}},
	}
	for name, tc := range cases {
		var req EmbeddingsRequest
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		cr := req.ToCore()
		if cr.Model != "m" || len(cr.Input) != len(tc.want) {
			t.Fatalf("%s: ToCore = %+v, want inputs %v", name, cr, tc.want)
		}
		for i, in := range tc.want {
			if cr.Input[i] != in {
				t.Fatalf("%s: input[%d] = %q, want %q", name, i, cr.Input[i], in)
			}
		}
	}
}

// TestFromCoreEmbeddings_floatShape: the float (default) response carries the
// OpenAI list shape with per-input index, the echoed model, and usage.
func TestFromCoreEmbeddings_floatShape(t *testing.T) {
	resp := FromCoreEmbeddings("my-model", "float", core.EmbedResponse{
		Embeddings: [][]float32{{1, 2}, {3, 4}},
		Usage:      core.Usage{InputTokens: 5},
	})
	if resp.Object != "list" || resp.Model != "my-model" {
		t.Fatalf("envelope = %+v, want list/my-model", resp)
	}
	if len(resp.Data) != 2 || resp.Data[1].Index != 1 || resp.Data[1].Object != "embedding" {
		t.Fatalf("data = %+v, want two indexed embedding objects", resp.Data)
	}
	// embedding should be a JSON float array
	var vec []float32
	if err := json.Unmarshal(resp.Data[0].Embedding, &vec); err != nil {
		t.Fatalf("embedding[0] is not a float array: %v", err)
	}
	if len(vec) != 2 || vec[0] != 1 || vec[1] != 2 {
		t.Fatalf("embedding[0] = %v, want [1 2]", vec)
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want 5/5", resp.Usage)
	}
}

// TestFromCoreEmbeddings_emptyFormatIsFloat: omitting encoding_format behaves like
// "float" (the OpenAI spec default for non-SDK callers).
func TestFromCoreEmbeddings_emptyFormatIsFloat(t *testing.T) {
	resp := FromCoreEmbeddings("m", "", core.EmbedResponse{
		Embeddings: [][]float32{{0.5}},
	})
	var vec []float32
	if err := json.Unmarshal(resp.Data[0].Embedding, &vec); err != nil {
		t.Fatalf("embedding is not a float array: %v", err)
	}
	if len(vec) != 1 || vec[0] != 0.5 {
		t.Fatalf("vec = %v, want [0.5]", vec)
	}
}

// TestFromCoreEmbeddings_base64: encoding_format:"base64" returns a JSON string
// containing the standard base64 encoding of the little-endian IEEE 754 float32
// bytes — the shape the OpenAI Python SDK expects by default.
func TestFromCoreEmbeddings_base64(t *testing.T) {
	vecs := []float32{1.0, 2.0, 3.0}
	resp := FromCoreEmbeddings("m", "base64", core.EmbedResponse{
		Embeddings: [][]float32{vecs},
	})

	// Embedding field must be a JSON string, not an array.
	var encoded string
	if err := json.Unmarshal(resp.Data[0].Embedding, &encoded); err != nil {
		t.Fatalf("embedding is not a JSON string: %v (raw: %s)", err, resp.Data[0].Embedding)
	}

	// Decode and verify each float round-trips as little-endian float32 bytes.
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if want := len(vecs) * 4; len(b) != want {
		t.Fatalf("decoded byte length = %d, want %d", len(b), want)
	}
	for i, want := range vecs {
		got := math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
		if got != want {
			t.Fatalf("float[%d] = %v, want %v", i, got, want)
		}
	}
}
