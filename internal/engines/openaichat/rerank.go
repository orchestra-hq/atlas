package openaichat

import (
	"context"
	"sort"

	"github.com/orchestra-hq/atlas/internal/core"
)

// rerankRequest is the Cohere-shaped /v1/rerank request body, the de-facto rerank
// convention vLLM/SGLang/llama.cpp emulate (there is no OpenAI rerank standard).
// top_n is forwarded so an engine that honors it can cap work; Rerank also enforces
// the cap itself, so the result is correct whether the engine respects it or not.
type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

// rerankResponse is the Cohere-shaped response. relevance_score is the Cohere field;
// score is accepted as a fallback for engines that use it, so one client covers both.
type rerankResponse struct {
	Results []rerankResult `json:"results"`
	Usage   struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type rerankResult struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
	Score          *float64 `json:"score"`
}

// score returns the result's relevance, preferring the Cohere relevance_score and
// falling back to a bare score field.
func (r rerankResult) score() float64 {
	switch {
	case r.RelevanceScore != nil:
		return *r.RelevanceScore
	case r.Score != nil:
		return *r.Score
	default:
		return 0
	}
}

// Rerank calls the engine's Cohere-compatible /v1/rerank endpoint and maps the
// result back to core (M3 phase 2b, ADR-0012). Atlas computes nothing — it forwards
// the query and documents to the engine and shapes the response — so this is the
// rerank sibling of Embed, shared by every adapter that embeds a Client. Results are
// sorted by descending relevance and capped to TopN, so ordering and the cap are
// guaranteed regardless of what the engine returns. An engine not launched in
// reranking mode answers non-200, which PostJSON surfaces as core.ErrEngineUnavailable
// (the gateway maps it to a retryable 529).
func (c *Client) Rerank(ctx context.Context, req core.RerankRequest) (core.RerankResponse, error) {
	if len(req.Documents) == 0 {
		return core.RerankResponse{}, nil
	}
	var out rerankResponse
	if err := c.PostJSON(ctx, "/v1/rerank", rerankRequest{
		Model:     c.model,
		Query:     req.Query,
		Documents: req.Documents,
		TopN:      req.TopN,
	}, &out); err != nil {
		return core.RerankResponse{}, err
	}
	results := make([]core.RerankResult, 0, len(out.Results))
	for _, r := range out.Results {
		// Drop any out-of-range index an engine might return rather than letting it
		// point a client past its own document list.
		if r.Index < 0 || r.Index >= len(req.Documents) {
			continue
		}
		results = append(results, core.RerankResult{Index: r.Index, Score: r.score()})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if req.TopN > 0 && len(results) > req.TopN {
		results = results[:req.TopN]
	}
	return core.RerankResponse{
		Results: results,
		Usage:   core.Usage{InputTokens: out.Usage.PromptTokens},
	}, nil
}
