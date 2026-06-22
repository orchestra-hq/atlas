package core

// EmbedRequest is the engine-independent embeddings request (M3 phase 2a,
// ADR-0012): one or more input strings to embed with the named model. Unlike a
// chat Request it has no messages, sampling, tools, or streaming — embedding is a
// single-shot text→vector mapping, so the gateway treats it as one admission unit
// with no tool loop. Wire handlers (internal/api/openai) produce it; engine
// adapters (internal/engines) consume it.
type EmbedRequest struct {
	Model string
	Input []string
}

// EmbedResponse is the engine-independent embeddings result: one vector per input
// in request order, plus the engine's token accounting (embeddings produce no
// output tokens, so only Usage.InputTokens is meaningful). Vectors are float32 to
// match what the engines return and to keep the payload compact over the worker
// channel.
type EmbedResponse struct {
	Embeddings [][]float32
	Usage      Usage
}
