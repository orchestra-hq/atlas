package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/orchestra-hq/atlas/internal/core"
)

// MessagesRequest is the POST /v1/messages wire request. Fields Atlas does not
// yet honor (metadata, …) are intentionally absent and land with their
// build-plan phases; unknown JSON keys are ignored rather than rejected so
// forward-compatible clients keep working.
type MessagesRequest struct {
	Model         string          `json:"model"`
	System        StringOrBlocks  `json:"system"`
	Messages      []WireMessage   `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Tools         []WireTool      `json:"tools"`
	ToolChoice    *WireToolChoice `json:"tool_choice"`
	Thinking      *WireThinking   `json:"thinking"`
	Stream        bool            `json:"stream"`
}

// WireThinking is the request's thinking object (ADR-0005). Type is "enabled"
// or "disabled"; budget_tokens is advisory and only meaningful when enabled.
type WireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// WireTool is one entry of the request's tools array.
type WireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// WireToolChoice is the request's tool_choice object. Name is set only for
// type "tool"; disable_parallel_tool_use is accepted and ignored.
type WireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// WireMessage is one turn on the wire. Content is a string or a list of blocks.
type WireMessage struct {
	Role    string         `json:"role"`
	Content StringOrBlocks `json:"content"`
}

// WireBlock is a content block on the wire. The fields used depend on Type:
// text uses Text; tool_use uses ID/Name/Input; tool_result uses
// ToolUseID/Content/IsError. MarshalJSON emits only the relevant subset so
// responses carry exactly the shape clients expect.
type WireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string         `json:"tool_use_id"`
	Content   StringOrBlocks `json:"content"`
	IsError   bool           `json:"is_error"`

	// thinking
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// MarshalJSON renders a WireBlock with only the fields its Type uses. Atlas
// emits text and tool_use blocks in responses; the others marshal for
// completeness (tests, request round-trips).
func (b WireBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{b.Type, b.ID, b.Name, input})
	case "tool_result":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error,omitempty"`
		}{b.Type, b.ToolUseID, b.Content.Text(), b.IsError})
	case "thinking":
		// Atlas does not sign reasoning (ADR-0005), so signature is emitted only
		// when present from an echoed-back block, never on freshly produced ones.
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature,omitempty"`
		}{b.Type, b.Thinking, b.Signature})
	default:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	}
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
			switch b.Type {
			case "text":
				blocks = append(blocks, core.TextBlock(b.Text))
			case "tool_use":
				blocks = append(blocks, core.ToolUseBlock(b.ID, b.Name, b.Input))
			case "tool_result":
				blocks = append(blocks, core.ToolResultBlock(b.ToolUseID, b.Content.Text(), b.IsError))
			case "thinking":
				blocks = append(blocks, core.ThinkingBlock(b.Thinking, b.Signature))
			default:
				return core.Request{}, ErrInvalid("messages[%d].content[%d].type: unsupported block type %q", i, j, b.Type)
			}
		}
		msgs = append(msgs, core.Message{Role: role, Blocks: blocks})
	}

	tools, err := toolsToCore(r.Tools)
	if err != nil {
		return core.Request{}, err
	}
	choice, err := toolChoiceToCore(r.ToolChoice)
	if err != nil {
		return core.Request{}, err
	}
	thinking, err := thinkingToCore(r.Thinking)
	if err != nil {
		return core.Request{}, err
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
		Tools:         tools,
		ToolChoice:    choice,
		Thinking:      thinking,
	}, nil
}

// thinkingToCore validates and translates the request's thinking object. A nil
// input (field absent) means the client did not ask for thinking. budget_tokens
// is advisory (ADR-0005) and not range-checked: Atlas never enforces it, so a
// small value is accepted rather than rejected the way the upstream API would.
func thinkingToCore(wire *WireThinking) (*core.ThinkingConfig, error) {
	if wire == nil {
		return nil, nil
	}
	switch wire.Type {
	case "enabled":
		return &core.ThinkingConfig{Enabled: true, BudgetTokens: wire.BudgetTokens}, nil
	case "disabled":
		return &core.ThinkingConfig{Enabled: false}, nil
	default:
		return nil, ErrInvalid("thinking.type: must be %q or %q", "enabled", "disabled")
	}
}

// toolsToCore validates and translates the request's tools array.
func toolsToCore(wire []WireTool) ([]core.Tool, error) {
	if len(wire) == 0 {
		return nil, nil
	}
	tools := make([]core.Tool, 0, len(wire))
	for i, t := range wire {
		if t.Name == "" {
			return nil, ErrInvalid("tools[%d].name: field required", i)
		}
		tools = append(tools, core.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return tools, nil
}

// toolChoiceToCore validates and translates the request's tool_choice. A nil
// input (field absent) means no constraint.
func toolChoiceToCore(wire *WireToolChoice) (*core.ToolChoice, error) {
	if wire == nil {
		return nil, nil
	}
	t := core.ToolChoiceType(wire.Type)
	switch t {
	case core.ToolChoiceAuto, core.ToolChoiceAny, core.ToolChoiceNone:
		return &core.ToolChoice{Type: t}, nil
	case core.ToolChoiceTool:
		if wire.Name == "" {
			return nil, ErrInvalid("tool_choice.name: field required when type is %q", core.ToolChoiceTool)
		}
		return &core.ToolChoice{Type: t, Name: wire.Name}, nil
	default:
		return nil, ErrInvalid("tool_choice.type: must be one of auto, any, tool, none")
	}
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
		switch b.Type {
		case core.BlockText:
			content = append(content, WireBlock{Type: "text", Text: b.Text})
		case core.BlockToolUse:
			content = append(content, WireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
		case core.BlockThinking:
			content = append(content, WireBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature})
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
