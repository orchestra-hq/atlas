package openai

import (
	"encoding/json"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// TestRerankRequest_ToCore: the Cohere-shaped request maps to the core form.
func TestRerankRequest_ToCore(t *testing.T) {
	var req RerankRequest
	if err := json.Unmarshal([]byte(`{"model":"rr","query":"q","documents":["a","b"],"top_n":1}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cr := req.ToCore()
	if cr.Model != "rr" || cr.Query != "q" || len(cr.Documents) != 2 || cr.TopN != 1 {
		t.Fatalf("ToCore = %+v", cr)
	}
}

// TestFromCoreRerank_echoesDocuments: with docs supplied (return_documents), each
// result carries its source text looked up by index; without them, Document is nil.
func TestFromCoreRerank_echoesDocuments(t *testing.T) {
	cresp := core.RerankResponse{
		Results: []core.RerankResult{{Index: 1, Score: 0.9}, {Index: 0, Score: 0.2}},
		Usage:   core.Usage{InputTokens: 4},
	}
	docs := []string{"first", "second"}

	with := FromCoreRerank("rr", cresp, docs)
	if with.Results[0].Document == nil || with.Results[0].Document.Text != "second" {
		t.Fatalf("expected echoed document 'second', got %+v", with.Results[0])
	}
	if with.Usage.PromptTokens != 4 || with.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %+v, want 4/4", with.Usage)
	}

	without := FromCoreRerank("rr", cresp, nil)
	if without.Results[0].Document != nil {
		t.Fatalf("expected no echoed document, got %+v", without.Results[0].Document)
	}
	if without.Results[0].Index != 1 || without.Results[0].RelevanceScore != 0.9 {
		t.Fatalf("top result = %+v, want index 1 score 0.9", without.Results[0])
	}
}
