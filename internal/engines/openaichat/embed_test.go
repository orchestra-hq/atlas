package openaichat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// TestEmbed_mapsVectorsAndUsage: the shared client posts to /v1/embeddings and maps
// the OpenAI response back to core, preserving usage.
func TestEmbed_mapsVectorsAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.EncodingFormat != "float" {
			t.Errorf("encoding_format = %q, want float", req.EncodingFormat)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float32{0.1, 0.2}},
				{"object": "embedding", "index": 1, "embedding": []float32{0.3, 0.4}},
			},
			"usage": map[string]any{"prompt_tokens": 9, "total_tokens": 9},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Embed(context.Background(), core.EmbedRequest{Input: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embeddings) != 2 || resp.Embeddings[1][0] != 0.3 {
		t.Fatalf("embeddings = %v, want two vectors", resp.Embeddings)
	}
	if resp.Usage.InputTokens != 9 {
		t.Fatalf("usage input = %d, want 9", resp.Usage.InputTokens)
	}
}

// TestEmbed_restoresInputOrder: the spec lets an engine return data in any order;
// Embed sorts by index so vectors line up with inputs.
func TestEmbed_restoresInputOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 2, "embedding": []float32{2}},
				{"index": 0, "embedding": []float32{0}},
				{"index": 1, "embedding": []float32{1}},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Embed(context.Background(), core.EmbedRequest{Input: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i := range resp.Embeddings {
		if resp.Embeddings[i][0] != float32(i) {
			t.Fatalf("vector %d = %v, want index-aligned %d", i, resp.Embeddings[i], i)
		}
	}
}

// TestEmbed_countMismatchErrors: a response with the wrong number of vectors is an
// error, not a silent truncation.
func TestEmbed_countMismatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{1}}},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	if _, err := c.Embed(context.Background(), core.EmbedRequest{Input: []string{"a", "b"}}); err == nil {
		t.Fatal("expected an error for a vector/input count mismatch")
	}
}

// TestEmbed_rejectsNonContiguousIndex: a response whose count matches the input but
// whose indices duplicate or skip a position (here [0,0,2] for three inputs) cannot be
// aligned to the inputs. Embed must error rather than return a silently misaligned
// vector — the count check alone would let this through.
func TestEmbed_rejectsNonContiguousIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{0}},
				{"index": 0, "embedding": []float32{9}}, // duplicate index 0
				{"index": 2, "embedding": []float32{2}}, // gap: no index 1
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	if _, err := c.Embed(context.Background(), core.EmbedRequest{Input: []string{"a", "b", "c"}}); err == nil {
		t.Fatal("expected an error for duplicate/gapped engine indices; vectors would be silently misaligned")
	}
}

// TestEmbed_emptyInputShortCircuits: no inputs means no engine call.
func TestEmbed_emptyInputShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("engine should not be called for empty input")
	}))
	defer srv.Close()

	c := NewClient("test", srv.URL, "m", false, srv.Client())
	resp, err := c.Embed(context.Background(), core.EmbedRequest{})
	if err != nil || len(resp.Embeddings) != 0 {
		t.Fatalf("empty input: got (%v, %v), want ({}, nil)", resp, err)
	}
}
