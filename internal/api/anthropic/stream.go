package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
)

// StreamWriter emits the Anthropic Messages streaming event sequence over an
// HTTP response as Server-Sent Events. A response is a series of content blocks
// (thinking, text, and tool_use), each framed by content_block_start …
// content_block_stop:
//
//	message_start
//	content_block_start   (index 0, thinking block)
//	content_block_delta*  (thinking_delta chunks)
//	content_block_stop    (index 0)
//	content_block_start   (index 1, text block)
//	content_block_delta*  (text_delta chunks)
//	content_block_stop    (index 1)
//	content_block_start   (index 2, tool_use block)
//	content_block_delta*  (input_json_delta chunks)
//	content_block_stop    (index 2)
//	message_delta         (stop_reason + final usage)
//	message_stop
//
// Blocks are opened lazily as content arrives: the caller drives it with
// NewStreamWriter, Start, then ThinkingDelta / TextDelta / ToolUseStart+
// ToolUseDelta as the model produces them, then Finish. Reasoning precedes the
// answer, so thinking deltas arrive before text. The writer assigns block
// indices and closes the open block when the next one starts (one block is open
// at a time, which is what the Anthropic wire requires). Each event is flushed
// immediately so clients see incremental output.
type StreamWriter struct {
	w     http.ResponseWriter
	flush func()
	id    string
	model string

	next     int    // next content-block index to assign
	open     bool   // a content block is currently open
	openIdx  int    // its index
	openKind string // "text" or "tool_use"
}

// NewStreamWriter sets the SSE response headers and returns a writer bound to
// the message id and model echoed in every event. It fails only if the
// ResponseWriter cannot flush (no chunked transfer), which would make
// streaming impossible.
func NewStreamWriter(w http.ResponseWriter, id, model string) (*StreamWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("anthropic: response writer does not support streaming")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &StreamWriter{w: w, flush: flusher.Flush, id: id, model: model}, nil
}

// Start emits message_start. inputTokens seeds the message_start usage (0 when
// not yet known from the engine; the final count is restated in Finish). No
// content block is opened yet — the first TextDelta or ToolUseStart opens one.
func (s *StreamWriter) Start(inputTokens int) error {
	start := messageStartEvent{Type: "message_start"}
	start.Message.ID = s.id
	start.Message.Type = "message"
	start.Message.Role = "assistant"
	start.Message.Model = s.model
	start.Message.Content = []WireBlock{}
	start.Message.Usage = WireUsage{InputTokens: inputTokens, OutputTokens: 0}
	return s.event("message_start", start)
}

// ThinkingDelta emits one content_block_delta carrying a thinking_delta,
// opening a thinking block first if one is not already open. Empty deltas are
// dropped so the wire never carries no-op events.
func (s *StreamWriter) ThinkingDelta(text string) error {
	if text == "" {
		return nil
	}
	if !s.open || s.openKind != "thinking" {
		if err := s.startBlock("thinking", WireBlock{Type: "thinking", Thinking: ""}); err != nil {
			return err
		}
	}
	return s.event("content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.openIdx,
		Delta: thinkingDelta{Type: "thinking_delta", Thinking: text},
	})
}

// TextDelta emits one content_block_delta carrying a text_delta, opening a text
// block first if one is not already open. Empty deltas are dropped so the wire
// never carries no-op events.
func (s *StreamWriter) TextDelta(text string) error {
	if text == "" {
		return nil
	}
	if !s.open || s.openKind != "text" {
		if err := s.startBlock("text", WireBlock{Type: "text", Text: ""}); err != nil {
			return err
		}
	}
	return s.event("content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.openIdx,
		Delta: textDelta{Type: "text_delta", Text: text},
	})
}

// ToolUseStart opens a tool_use content block for a call with the given id and
// tool name, closing any block already open. Its JSON arguments follow as
// ToolUseDelta fragments.
func (s *StreamWriter) ToolUseStart(id, name string) error {
	return s.startBlock("tool_use", WireBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage("{}")})
}

// ToolUseDelta emits one content_block_delta carrying an input_json_delta
// fragment of the open tool_use block's arguments. Empty fragments are dropped.
func (s *StreamWriter) ToolUseDelta(partialJSON string) error {
	if partialJSON == "" {
		return nil
	}
	return s.event("content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.openIdx,
		Delta: inputJSONDelta{Type: "input_json_delta", PartialJSON: partialJSON},
	})
}

// Finish closes the open block and the message: a trailing content_block_stop,
// then message_delta (stop reason, stop sequence, final usage) and message_stop.
// If no block was ever opened (empty generation), it emits an empty text block
// so the response always carries at least one content block.
func (s *StreamWriter) Finish(reason core.StopReason, stopSeq *string, usage core.Usage) error {
	if !s.open && s.next == 0 {
		if err := s.startBlock("text", WireBlock{Type: "text", Text: ""}); err != nil {
			return err
		}
	}
	if err := s.closeBlock(); err != nil {
		return err
	}

	if err := s.event("message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Delta: messageDelta{StopReason: string(reason), StopSequence: stopSeq},
		Usage: WireUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}); err != nil {
		return err
	}

	return s.event("message_stop", messageStopEvent{Type: "message_stop"})
}

// startBlock closes any open block, then emits content_block_start for a new
// block of the given kind at the next index.
func (s *StreamWriter) startBlock(kind string, block WireBlock) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.openIdx = s.next
	s.next++
	s.open = true
	s.openKind = kind
	return s.event("content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        s.openIdx,
		ContentBlock: block,
	})
}

// closeBlock emits content_block_stop for the open block, if any.
func (s *StreamWriter) closeBlock() error {
	if !s.open {
		return nil
	}
	s.open = false
	return s.event("content_block_stop", contentBlockStopEvent{Type: "content_block_stop", Index: s.openIdx})
}

// Error emits an error event mid-stream, for failures after the response
// headers are already committed (a normal HTTP error is impossible by then).
func (s *StreamWriter) Error(errType ErrorType, msg string) error {
	return s.event("error", errorEnvelope{Type: "error", Error: errorBody{Type: errType, Message: msg}})
}

// event marshals payload and writes one SSE event, then flushes.
func (s *StreamWriter) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	s.flush()
	return nil
}

// Event payload types, named to match the Anthropic streaming schema.

type messageStartEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID           string      `json:"id"`
		Type         string      `json:"type"`
		Role         string      `json:"role"`
		Model        string      `json:"model"`
		Content      []WireBlock `json:"content"`
		StopReason   *string     `json:"stop_reason"`
		StopSequence *string     `json:"stop_sequence"`
		Usage        WireUsage   `json:"usage"`
	} `json:"message"`
}

type contentBlockStartEvent struct {
	Type         string    `json:"type"`
	Index        int       `json:"index"`
	ContentBlock WireBlock `json:"content_block"`
}

type contentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"` // thinkingDelta, textDelta, or inputJSONDelta
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string       `json:"type"`
	Delta messageDelta `json:"delta"`
	Usage WireUsage    `json:"usage"`
}

type messageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}
