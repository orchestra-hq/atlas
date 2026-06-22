package openai

import "github.com/orchestra-hq/atlas/internal/core"

// RerankRequest is the POST /v1/rerank request. Rerank has no OpenAI standard, so
// Atlas serves a native endpoint following the de-facto Cohere shape (query +
// documents + top_n → relevance-ordered results), which vLLM/SGLang/llama.cpp
// already emulate. ReturnDocuments echoes each result's source text when set.
type RerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents,omitempty"`
}

// ToCore maps the request to the engine-independent core form. Documents are
// retained on the request value so the handler can echo them when ReturnDocuments
// is set; core carries only what the engine needs to score.
func (r RerankRequest) ToCore() core.RerankRequest {
	return core.RerankRequest{Model: r.Model, Query: r.Query, Documents: r.Documents, TopN: r.TopN}
}

// RerankResponse is the Cohere-shaped response: results ordered by descending
// relevance, each carrying its original document index and score.
type RerankResponse struct {
	Model   string         `json:"model"`
	Results []RerankResult `json:"results"`
	Usage   RerankUsage    `json:"usage"`
}

// RerankResult is one scored document. Document is populated only when the request
// set return_documents.
type RerankResult struct {
	Index          int       `json:"index"`
	RelevanceScore float64   `json:"relevance_score"`
	Document       *Document `json:"document,omitempty"`
}

// Document is a result's echoed source text (Cohere shape).
type Document struct {
	Text string `json:"text"`
}

// RerankUsage is the rerank token accounting (no completion tokens).
type RerankUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// FromCoreRerank shapes a core rerank result into the response, echoing the model
// name and — when docs is non-nil (the request set return_documents) — each result's
// source text looked up by its original index.
func FromCoreRerank(model string, resp core.RerankResponse, docs []string) RerankResponse {
	results := make([]RerankResult, len(resp.Results))
	for i, r := range resp.Results {
		out := RerankResult{Index: r.Index, RelevanceScore: r.Score}
		if docs != nil && r.Index >= 0 && r.Index < len(docs) {
			out.Document = &Document{Text: docs[r.Index]}
		}
		results[i] = out
	}
	return RerankResponse{
		Model:   model,
		Results: results,
		Usage: RerankUsage{
			PromptTokens: resp.Usage.InputTokens,
			TotalTokens:  resp.Usage.InputTokens,
		},
	}
}
