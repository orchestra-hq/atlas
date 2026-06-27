// Package openai holds the wire types for Atlas's OpenAI-compatible surface
// (POST /v1/chat/completions — see docs/internal/api-surface.md) and the translation
// between those shapes and internal/core. Per build-time decision 1 in
// docs/internal/m0-build-plan.md the gateway owns this surface: it translates
// core⇄OpenAI wire itself rather than proxying an engine's endpoint, so one
// set of semantics holds across every engine.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/orchestra-hq/atlas/internal/core"
)

// ChatCompletionRequest is the POST /v1/chat/completions wire request. Fields
// Atlas does not honor yet are intentionally absent and unknown JSON keys are
// ignored, so forward-compatible clients keep working. There is no thinking
// parameter: the OpenAI chat surface has no portable reasoning toggle, so
// reasoning output is suppressed here (the Anthropic surface carries thinking).
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	// MaxTokens is the legacy field; MaxCompletionTokens is its modern alias.
	// Either satisfies the required max-tokens bound (core needs a positive cap).
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                StringOrStrings `json:"stop"`
	Tools               []Tool          `json:"tools"`
	ToolChoice          *ToolChoice     `json:"tool_choice"`
	Stream              bool            `json:"stream"`
	StreamOptions       *StreamOptions  `json:"stream_options"`
}

// StreamOptions carries OpenAI's stream_options. include_usage asks for a final
// usage-only chunk after the content chunks.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is one message on the wire. role is system/user/assistant/tool;
// the other fields are populated per role: assistant turns may carry tool_calls,
// tool turns carry tool_call_id.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	ToolCallID string     `json:"tool_call_id"`
	Name       string     `json:"name"`
}

// ToolCall is one OpenAI tool call (request and response share this shape).
// Arguments is the call's JSON arguments as a string.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc is the function payload of a tool call.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is one entry of the request's tools array.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function definition of a tool. Parameters is a JSON
// Schema object carried raw and passed to the engine unchanged.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolChoice is the request's tool_choice: a bare string ("auto"/"none"/
// "required") or a {type:"function",function:{name}} object naming one tool.
type ToolChoice struct {
	Mode string // "auto", "none", "required", or "" when a specific tool is named
	Name string // set only for a specific-function choice
}

// UnmarshalJSON accepts either form of tool_choice.
func (t *ToolChoice) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		t.Mode = s
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	t.Name = obj.Function.Name
	return nil
}

// Content is an OpenAI message content field: a bare string, or an array of
// content parts (multimodal). Atlas's M0 engines are text-only, so it
// normalizes both into concatenated text.
type Content struct {
	Text   string
	IsNull bool
}

// UnmarshalJSON accepts a string, an array of parts, or null.
func (c *Content) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		c.IsNull = true
		return nil
	}
	switch data[0] {
	case '"':
		return json.Unmarshal(data, &c.Text)
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		c.Text = b.String()
		return nil
	default:
		return fmt.Errorf("content must be a string or array, got %q", string(data[:1]))
	}
}

// ToCore validates the wire request and translates it into a core.Request.
// Validation mirrors the OpenAI API surface enough that SDKs raise their typed
// errors: missing model/messages and a non-positive token cap return a 400.
func (r *ChatCompletionRequest) ToCore() (core.Request, error) {
	if r.Model == "" {
		return core.Request{}, ErrInvalid("you must provide a model parameter")
	}
	if len(r.Messages) == 0 {
		return core.Request{}, ErrInvalid("messages: non-empty list required")
	}

	maxTokens := r.MaxCompletionTokens
	if maxTokens == 0 {
		maxTokens = r.MaxTokens
	}
	if maxTokens == 0 {
		// OpenAI defaults to the model's remaining context when omitted. Atlas's
		// core requires a positive cap (it backs the context-window assertion),
		// so apply a generous default rather than rejecting the request.
		maxTokens = defaultMaxTokens
	}
	if maxTokens < 1 {
		return core.Request{}, ErrInvalid("max_tokens: integer >= 1 required")
	}

	var system strings.Builder
	msgs := make([]core.Message, 0, len(r.Messages))
	for i, m := range r.Messages {
		switch m.Role {
		case "system", "developer":
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(m.Content.Text)
		case "user":
			msgs = append(msgs, core.Message{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock(m.Content.Text)}})
		case "assistant":
			blocks := make([]core.ContentBlock, 0, 1+len(m.ToolCalls))
			if m.Content.Text != "" {
				blocks = append(blocks, core.TextBlock(m.Content.Text))
			}
			for j, tc := range m.ToolCalls {
				if tc.ID == "" {
					return core.Request{}, ErrInvalid("messages[%d].tool_calls[%d].id: field required", i, j)
				}
				blocks = append(blocks, core.ToolUseBlock(tc.ID, tc.Function.Name, toolArgs(tc.Function.Arguments)))
			}
			msgs = append(msgs, core.Message{Role: core.RoleAssistant, Blocks: blocks})
		case "tool":
			// A tool result is its own turn in core (a user-role tool_result
			// block); the engine adapter re-expands it to a role:"tool" message.
			if m.ToolCallID == "" {
				return core.Request{}, ErrInvalid("messages[%d].tool_call_id: field required for a tool message", i)
			}
			msgs = append(msgs, core.Message{Role: core.RoleUser, Blocks: []core.ContentBlock{
				core.ToolResultBlock(m.ToolCallID, m.Content.Text, false),
			}})
		default:
			return core.Request{}, ErrInvalid("messages[%d].role: unsupported role %q", i, m.Role)
		}
	}

	tools, err := toolsToCore(r.Tools)
	if err != nil {
		return core.Request{}, err
	}
	choice, err := toolChoiceToCore(r.ToolChoice)
	if err != nil {
		return core.Request{}, err
	}

	return core.Request{
		Model:         r.Model,
		System:        system.String(),
		Messages:      msgs,
		MaxTokens:     maxTokens,
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		StopSequences: r.Stop.Values,
		Tools:         tools,
		ToolChoice:    choice,
	}, nil
}

// defaultMaxTokens caps generation when the request omits a token limit. Large
// enough not to surprise a chat client, small enough to keep the
// context-window assertion meaningful.
const defaultMaxTokens = 4096

// toolArgs normalizes a tool call's JSON arguments; an empty value becomes {}.
func toolArgs(arguments string) json.RawMessage {
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

// toolsToCore validates and translates the request's tools array.
func toolsToCore(wire []Tool) ([]core.Tool, error) {
	if len(wire) == 0 {
		return nil, nil
	}
	tools := make([]core.Tool, 0, len(wire))
	for i, t := range wire {
		if t.Function.Name == "" {
			return nil, ErrInvalid("tools[%d].function.name: field required", i)
		}
		tools = append(tools, core.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return tools, nil
}

// toolChoiceToCore validates and translates tool_choice. A nil input (field
// absent) means no constraint.
func toolChoiceToCore(wire *ToolChoice) (*core.ToolChoice, error) {
	if wire == nil {
		return nil, nil
	}
	if wire.Name != "" {
		return &core.ToolChoice{Type: core.ToolChoiceTool, Name: wire.Name}, nil
	}
	switch wire.Mode {
	case "auto":
		return &core.ToolChoice{Type: core.ToolChoiceAuto}, nil
	case "none":
		return &core.ToolChoice{Type: core.ToolChoiceNone}, nil
	case "required":
		return &core.ToolChoice{Type: core.ToolChoiceAny}, nil
	default:
		return nil, ErrInvalid("tool_choice: must be %q, %q, %q, or a {type:function} object", "auto", "none", "required")
	}
}

// StringOrStrings decodes OpenAI's stop field, which is a bare string or a list
// of strings. It normalizes both into Values.
type StringOrStrings struct {
	Values []string
}

// UnmarshalJSON accepts a string, a list of strings, or null.
func (s *StringOrStrings) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		s.Values = nil
		return nil
	}
	if data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		s.Values = []string{str}
		return nil
	}
	return json.Unmarshal(data, &s.Values)
}
