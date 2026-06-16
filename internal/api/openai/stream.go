package openai

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
)

// StreamWriter emits the OpenAI chat-completion streaming format over an HTTP
// response as Server-Sent Events. Every event is a chat.completion.chunk:
//
//	data: {... choices:[{delta:{role:"assistant"}}]}
//	data: {... choices:[{delta:{content:"Hello"}}]}
//	data: {... choices:[{delta:{tool_calls:[{index:0,id,function:{name,arguments}}]}}]}
//	data: {... choices:[{delta:{}, finish_reason:"stop"}]}
//	data: {... choices:[], usage:{...}}      (only with stream_options.include_usage)
//	data: [DONE]
//
// The caller drives it: NewStreamWriter, Role, then TextDelta / ToolCallStart+
// ToolCallDelta as the model produces output, then Finish. Tool-call argument
// deltas are grouped by a stable per-call index the SDKs accumulate on.
type StreamWriter struct {
	w       http.ResponseWriter
	flush   func()
	id      string
	created int64
	model   string

	toolIndex map[int]int // engine call index -> 0-based output index
}

// NewStreamWriter sets the SSE response headers and returns a writer bound to
// the completion id, created timestamp, and model echoed in every chunk. It
// fails only if the ResponseWriter cannot flush (no chunked transfer).
func NewStreamWriter(w http.ResponseWriter, id string, created int64, model string) (*StreamWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("openai: response writer does not support streaming")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &StreamWriter{w: w, flush: flusher.Flush, id: id, created: created, model: model, toolIndex: map[int]int{}}, nil
}

// Role emits the opening chunk that announces the assistant role, as OpenAI
// servers do before any content. SDKs expect the role on the first delta.
func (s *StreamWriter) Role() error {
	return s.chunk(delta{Role: "assistant"})
}

// TextDelta emits one content delta. Empty deltas are dropped.
func (s *StreamWriter) TextDelta(text string) error {
	if text == "" {
		return nil
	}
	return s.chunk(delta{Content: &text})
}

// ToolCallStart emits the opening delta for a tool call: its id, name, and an
// empty arguments string under a stable output index. Its JSON arguments follow
// as ToolCallDelta fragments.
func (s *StreamWriter) ToolCallStart(engineIndex int, id, name string) error {
	out := len(s.toolIndex)
	s.toolIndex[engineIndex] = out
	empty := ""
	return s.chunk(delta{ToolCalls: []deltaToolCall{{
		Index:    out,
		ID:       id,
		Type:     "function",
		Function: &deltaFunc{Name: name, Arguments: &empty},
	}}})
}

// ToolCallDelta emits one arguments fragment for the open tool call. Empty
// fragments are dropped.
func (s *StreamWriter) ToolCallDelta(engineIndex int, argsFragment string) error {
	if argsFragment == "" {
		return nil
	}
	out, ok := s.toolIndex[engineIndex]
	if !ok {
		// A delta before its start: announce the call with no name so the SDK
		// still groups the arguments correctly.
		out = len(s.toolIndex)
		s.toolIndex[engineIndex] = out
	}
	return s.chunk(delta{ToolCalls: []deltaToolCall{{
		Index:    out,
		Function: &deltaFunc{Arguments: &argsFragment},
	}}})
}

// Finish emits the terminal content chunk (empty delta + finish_reason), then —
// when includeUsage is set — a usage-only chunk, then the [DONE] sentinel.
func (s *StreamWriter) Finish(reason core.StopReason, usage core.Usage, includeUsage bool) error {
	fr := FinishReason(reason)
	if err := s.chunkRaw(chunkPayload{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{Index: 0, Delta: delta{}, FinishReason: &fr}},
	}); err != nil {
		return err
	}
	if includeUsage {
		u := Usage{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.InputTokens + usage.OutputTokens,
		}
		if err := s.chunkRaw(chunkPayload{
			ID:      s.id,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []chunkChoice{},
			Usage:   &u,
		}); err != nil {
			return err
		}
	}
	return s.done()
}

// Error reports a mid-stream failure (the response status is already committed,
// so a normal HTTP error is impossible). It emits an error envelope as a data
// event, then the [DONE] sentinel so the client's stream loop terminates.
func (s *StreamWriter) Error(errType ErrorType, msg string) error {
	if err := s.writeData(errorEnvelope{Error: errorBody{Message: msg, Type: errType}}); err != nil {
		return err
	}
	return s.done()
}

// chunk emits one chat.completion.chunk carrying a single choice with d.
func (s *StreamWriter) chunk(d delta) error {
	return s.chunkRaw(chunkPayload{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{Index: 0, Delta: d}},
	})
}

func (s *StreamWriter) chunkRaw(p chunkPayload) error {
	return s.writeData(p)
}

// writeData marshals payload and writes one SSE data event, then flushes.
func (s *StreamWriter) writeData(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.flush()
	return nil
}

// done writes the [DONE] terminator and flushes.
func (s *StreamWriter) done() error {
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.flush()
	return nil
}

// Streaming payload types, named to match the OpenAI chunk schema.

type chunkPayload struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	ToolCalls []deltaToolCall `json:"tool_calls,omitempty"`
}

type deltaToolCall struct {
	Index    int        `json:"index"`
	ID       string     `json:"id,omitempty"`
	Type     string     `json:"type,omitempty"`
	Function *deltaFunc `json:"function,omitempty"`
}

type deltaFunc struct {
	Name      string  `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}
