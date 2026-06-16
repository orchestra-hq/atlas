package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/orchestra-hq/atlas/internal/core"
)

// StreamWriter emits the Anthropic Messages streaming event sequence over an
// HTTP response as Server-Sent Events. The full sequence for a text response is:
//
//	message_start
//	content_block_start   (index 0, empty text block)
//	content_block_delta*  (text_delta chunks)
//	content_block_stop    (index 0)
//	message_delta         (stop_reason + final usage)
//	message_stop
//
// The caller drives it: NewStreamWriter, Start, TextDelta per chunk, then
// Finish. Each event is flushed immediately so clients see incremental output.
type StreamWriter struct {
	w     http.ResponseWriter
	flush func()
	id    string
	model string
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

// Start emits message_start followed by content_block_start for the single
// text block. inputTokens seeds the message_start usage (0 when not yet known
// from the engine; the final count is restated in Finish).
func (s *StreamWriter) Start(inputTokens int) error {
	start := messageStartEvent{Type: "message_start"}
	start.Message.ID = s.id
	start.Message.Type = "message"
	start.Message.Role = "assistant"
	start.Message.Model = s.model
	start.Message.Content = []WireBlock{}
	start.Message.Usage = WireUsage{InputTokens: inputTokens, OutputTokens: 0}
	if err := s.event("message_start", start); err != nil {
		return err
	}

	return s.event("content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: WireBlock{Type: "text", Text: ""},
	})
}

// TextDelta emits one content_block_delta carrying a text_delta. Empty deltas
// are dropped so the wire never carries no-op events.
func (s *StreamWriter) TextDelta(text string) error {
	if text == "" {
		return nil
	}
	return s.event("content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: 0,
		Delta: textDelta{Type: "text_delta", Text: text},
	})
}

// Finish closes the content block and the message: content_block_stop,
// message_delta (stop reason, stop sequence, final usage), then message_stop.
func (s *StreamWriter) Finish(reason core.StopReason, stopSeq *string, usage core.Usage) error {
	if err := s.event("content_block_stop", contentBlockStopEvent{
		Type:  "content_block_stop",
		Index: 0,
	}); err != nil {
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
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta textDelta `json:"delta"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
