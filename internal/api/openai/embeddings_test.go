package openai

import (
	"encoding/json"
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

// TestFromCoreEmbeddings_shape: the response carries the OpenAI list shape with
// per-input index, the echoed model, and usage.
func TestFromCoreEmbeddings_shape(t *testing.T) {
	resp := FromCoreEmbeddings("my-model", core.EmbedResponse{
		Embeddings: [][]float32{{1, 2}, {3, 4}},
		Usage:      core.Usage{InputTokens: 5},
	})
	if resp.Object != "list" || resp.Model != "my-model" {
		t.Fatalf("envelope = %+v, want list/my-model", resp)
	}
	if len(resp.Data) != 2 || resp.Data[1].Index != 1 || resp.Data[1].Object != "embedding" {
		t.Fatalf("data = %+v, want two indexed embedding objects", resp.Data)
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want 5/5", resp.Usage)
	}
}
