package core

// RerankRequest is the engine-independent rerank request (M3 phase 2b, ADR-0012):
// score each document against the query and return them ordered by relevance. Like
// EmbedRequest it is single-shot — no messages, sampling, tools, or streaming. TopN
// caps how many ranked results to return (0 = all). Wire handlers (internal/api)
// produce it; engine adapters (internal/engines) consume it.
type RerankRequest struct {
	Model     string
	Query     string
	Documents []string
	TopN      int
}

// RerankResponse is the engine-independent rerank result: the documents ordered by
// descending relevance (each carrying its original input index), plus the engine's
// token accounting (rerank produces no output tokens, so only Usage.InputTokens is
// meaningful).
type RerankResponse struct {
	Results []RerankResult
	Usage   Usage
}

// RerankResult is one scored document: Index is its position in the request's
// Documents, Score its relevance to the query (higher is more relevant).
type RerankResult struct {
	Index int
	Score float64
}
