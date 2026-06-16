package openai

import (
	"encoding/json"

	"github.com/orchestra-hq/atlas/internal/core"
)

// ChatCompletionResponse is the non-streaming POST /v1/chat/completions wire
// response. id/created/model are wire concerns the gateway supplies.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion choice. Atlas always returns exactly one.
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// ResponseMessage is the assistant message in a choice. Content is a pointer so
// it serializes as JSON null on a tool-call-only turn (OpenAI's shape), which
// the SDKs expect.
type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// FromCore builds the wire response from a core result. id, created, and model
// are supplied by the gateway. Text blocks concatenate into message content;
// tool_use blocks become tool_calls; the stop reason maps to finish_reason.
func FromCore(id string, created int64, model string, resp core.Response) ChatCompletionResponse {
	var text string
	var hasText bool
	var toolCalls []ToolCall
	for _, b := range resp.Blocks {
		switch b.Type {
		case core.BlockText:
			text += b.Text
			hasText = true
		case core.BlockToolUse:
			toolCalls = append(toolCalls, ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: ToolCallFunc{
					Name:      b.Name,
					Arguments: argsString(b.Input),
				},
			})
		}
		// Thinking blocks are not surfaced on the OpenAI chat surface.
	}

	msg := ResponseMessage{Role: "assistant", ToolCalls: toolCalls}
	// content is null only on a pure tool-call turn; otherwise (including an
	// empty answer) it is a string, which is what chat clients render.
	if hasText || len(toolCalls) == 0 {
		msg.Content = &text
	}

	return ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: FinishReason(resp.StopReason),
		}},
		Usage: Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

// FinishReason maps an Anthropic-vocabulary core stop reason onto the OpenAI
// finish_reason vocabulary. stop_sequence collapses to "stop" (OpenAI has no
// distinct value); an unknown reason defaults to "stop".
func FinishReason(reason core.StopReason) string {
	switch reason {
	case core.StopMaxTokens:
		return "length"
	case core.StopToolUse:
		return "tool_calls"
	case core.StopStopSequence, core.StopEndTurn:
		return "stop"
	default:
		return "stop"
	}
}

// argsString renders a tool call's raw JSON arguments as the string OpenAI
// carries; an empty value becomes "{}".
func argsString(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	return string(input)
}
