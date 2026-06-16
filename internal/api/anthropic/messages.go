package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/orchestra-hq/atlas/internal/core"
)

// MessagesRequest is the POST /v1/messages wire request. Fields Atlas does not
// yet honor (tools, thinking, metadata, …) are intentionally absent in phase 2
// and land with their build-plan phases; unknown JSON keys are ignored rather
// than rejected so forward-compatible clients keep working.
type MessagesRequest struct {
	Model         string         `json:"model"`
	System        StringOrBlocks `json:"system"`
	Messages      []WireMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens"`
	Temperature   *float64       `json:"temperature"`
	TopP          *float64       `json:"top_p"`
	TopK          *int           `json:"top_k"`
	StopSequences []string       `json:"stop_sequences"`
	Stream        bool           `json:"stream"`
}

// WireMessage is one turn on the wire. Content is a string or a list of blocks.
type WireMessage struct {
	Role    string         `json:"role"`
	Content StringOrBlocks `json:"content"`
}

// WireBlock is a content block on the wire. Phase 2 is text-only.
type WireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// StringOrBlocks decodes the Anthropic content field, which is either a bare
// string or a list of content blocks. It normalizes both into Blocks.
type StringOrBlocks struct {
	Blocks []WireBlock
}

// UnmarshalJSON accepts a string, a block list, or null.
func (s *StringOrBlocks) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		s.Blocks = nil
		return nil
	}
	switch data[0] {
	case '"':
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		s.Blocks = []WireBlock{{Type: "text", Text: str}}
		return nil
	case '[':
		return json.Unmarshal(data, &s.Blocks)
	default:
		return fmt.Errorf("content must be a string or array, got %q", firstByte(data))
	}
}

func firstByte(data []byte) string {
	return string(data[:1])
}

// Text concatenates the text of every text block.
func (s StringOrBlocks) Text() string {
	var out string
	for _, b := range s.Blocks {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

// ToCore validates the wire request and translates it into a core.Request.
// Validation mirrors the Anthropic API: missing model/max_tokens/messages and
// bad role/content shapes return a 400 invalid_request_error.
func (r *MessagesRequest) ToCore() (core.Request, error) {
	if r.Model == "" {
		return core.Request{}, ErrInvalid("model: field required")
	}
	if r.MaxTokens < 1 {
		return core.Request{}, ErrInvalid("max_tokens: integer >= 1 required")
	}
	if len(r.Messages) == 0 {
		return core.Request{}, ErrInvalid("messages: non-empty list required")
	}

	msgs := make([]core.Message, 0, len(r.Messages))
	for i, m := range r.Messages {
		role := core.Role(m.Role)
		if role != core.RoleUser && role != core.RoleAssistant {
			return core.Request{}, ErrInvalid("messages[%d].role: must be %q or %q", i, core.RoleUser, core.RoleAssistant)
		}
		blocks := make([]core.ContentBlock, 0, len(m.Content.Blocks))
		for j, b := range m.Content.Blocks {
			if b.Type != "text" {
				return core.Request{}, ErrInvalid("messages[%d].content[%d]: only text blocks are supported in this release", i, j)
			}
			blocks = append(blocks, core.TextBlock(b.Text))
		}
		msgs = append(msgs, core.Message{Role: role, Blocks: blocks})
	}

	return core.Request{
		Model:         r.Model,
		System:        r.System.Text(),
		Messages:      msgs,
		MaxTokens:     r.MaxTokens,
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		TopK:          r.TopK,
		StopSequences: r.StopSequences,
	}, nil
}

// MessagesResponse is the POST /v1/messages wire response.
type MessagesResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []WireBlock `json:"content"`
	StopReason   string      `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        WireUsage   `json:"usage"`
}

// WireUsage is the usage object in the response.
type WireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// FromCore builds the wire response from a core result. id and model are wire
// concerns the gateway supplies; stopSequence is non-nil only when the
// gateway stopped on one of the request's stop sequences.
func FromCore(id, model string, resp core.Response, stopSequence *string) MessagesResponse {
	content := make([]WireBlock, 0, len(resp.Blocks))
	for _, b := range resp.Blocks {
		if b.Type == core.BlockText {
			content = append(content, WireBlock{Type: "text", Text: b.Text})
		}
	}
	return MessagesResponse{
		ID:           id,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      content,
		StopReason:   string(resp.StopReason),
		StopSequence: stopSequence,
		Usage: WireUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}
}
