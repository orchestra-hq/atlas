package openaichat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// TestRerank_sortsDescAndCaps: the shared client posts to /v1/rerank, then sorts
// results by descending relevance and applies top_n, regardless of engine order.
func TestRerank_sortsDescAndCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("path = %s, want /v1/rerank", r.URL.Path)
		}
		var req rerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Query != "q" || len(req.Documents) != 3 {
			t.Errorf("forwarded request = %+v", req)
		}
		// Returned out of order, lowest first, to prove the client re-sorts.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.1},
				{"index": 2, "relevance_score": 0.9},
				{"index": 1, "relevance_score": 0.5},
			},
			"usage": map[string]any{"prompt_tokens": 12, "total_tokens": 12},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Rerank(context.Background(), core.RerankRequest{Query: "q", Documents: []string{"a", "b", "c"}, TopN: 2})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results length = %d, want 2 (top_n)", len(resp.Results))
	}
	if resp.Results[0].Index != 2 || resp.Results[1].Index != 1 {
		t.Fatalf("order = %+v, want indices [2 1] by descending score", resp.Results)
	}
	if resp.Usage.InputTokens != 12 {
		t.Fatalf("usage input = %d, want 12", resp.Usage.InputTokens)
	}
}

// TestRerank_scoreFallback: an engine that returns a bare "score" field instead of
// Cohere's "relevance_score" is still understood.
func TestRerank_scoreFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "score": 0.2},
				{"index": 1, "score": 0.8},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Rerank(context.Background(), core.RerankRequest{Query: "q", Documents: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if resp.Results[0].Index != 1 || resp.Results[0].Score != 0.8 {
		t.Fatalf("top result = %+v, want index 1 score 0.8", resp.Results[0])
	}
}

// TestRerank_dropsOutOfRangeIndex: a result whose index is outside the document list
// is discarded rather than handed to the client.
func TestRerank_dropsOutOfRangeIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.5},
				{"index": 9, "relevance_score": 0.99},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Rerank(context.Background(), core.RerankRequest{Query: "q", Documents: []string{"only-one"}})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Index != 0 {
		t.Fatalf("results = %+v, want just the in-range index 0", resp.Results)
	}
}

// TestRerank_emptyDocsShortCircuits: no documents means no engine call.
func TestRerank_emptyDocsShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("engine should not be called for empty documents")
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Rerank(context.Background(), core.RerankRequest{Query: "q"})
	if err != nil || len(resp.Results) != 0 {
		t.Fatalf("empty docs: got (%v, %v), want ({}, nil)", resp, err)
	}
}
