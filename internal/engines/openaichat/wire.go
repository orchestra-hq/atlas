// Package openaichat is the shared core⇄OpenAI chat-completions translation
// used by every engine adapter that speaks an OpenAI-compatible endpoint
// (llama.cpp, vLLM, and SGLang later). Per build-time decision 1 in
// docs/m0-build-plan.md the gateway owns all Anthropic semantics; adapters only
// translate to and from the engine's OpenAI wire, and that translation is
// identical across engines — so it lives here once. Engine-specific endpoints
// (token counting, context window) stay in each adapter.
package openaichat

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/orchestra-hq/atlas/internal/core"
)

// Message is one OpenAI chat message. Content is omitted on an assistant turn
// that only carries tool calls (OpenAI accepts a missing/empty content there);
// ToolCallID links a role:"tool" message to the call it answers.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one OpenAI tool call (request and response shape coincide).
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is one entry of the OpenAI tools array.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// Request is the OpenAI /v1/chat/completions request Atlas sends an engine.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	TopK        *int      `json:"top_k,omitempty"` // llama.cpp/vLLM extension, ignored by strict servers
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"` // string or {type,function} object
	// ChatTemplateKwargs are passed to the engine's Jinja chat template. Atlas
	// uses it to toggle reasoning on hybrid-thinking models (ADR-0005); see
	// ThinkingKwargs.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	Stream             bool           `json:"stream"`
	StreamOptions      *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions asks the OpenAI-compat server to include a final usage chunk
// when streaming.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Response is the non-streaming /v1/chat/completions response.
type Response struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// ReasoningContent is the model's reasoning trace, separated by the
			// engine's reasoning parser (the de-facto field name shared by
			// llama.cpp, vLLM, and SGLang). Empty for non-reasoning models.
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// StreamChunk is one Server-Sent Event from the streaming endpoint. Deltas
// carry incremental content; the terminal chunk carries a finish_reason and
// (with stream_options.include_usage) a usage object.
type StreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// buildRequest translates a core request into the OpenAI chat request the client
// sends. Stop sequences are intentionally not forwarded: the gateway owns that
// semantics (see server.Gateway). It is a method so the thinking kwarg can
// consult the model's reasoning capability (M2 phase 4b).
func (c *Client) buildRequest(req core.Request, stream bool) Request {
	r := Request{
		Model:              c.model,
		Messages:           Messages(req),
		MaxTokens:          req.MaxTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		TopK:               req.TopK,
		Tools:              Tools(req.Tools),
		ToolChoice:         MapToolChoice(req.ToolChoice),
		ChatTemplateKwargs: c.ThinkingKwargs(req),
		Stream:             stream,
	}
	if stream {
		r.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	return r
}

// Messages expands a core request's system prompt and turns into the OpenAI
// chat message list. Shared by generation (BuildRequest) and token counting so
// a count and the identical generation map to the same prompt.
func Messages(req core.Request) []Message {
	msgs := make([]Message, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, Message{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, expand(m)...)
	}
	return msgs
}

// expand turns one core message into OpenAI chat messages. Most map one-to-one,
// but a user turn carrying tool_result blocks becomes a separate role:"tool"
// message per result (OpenAI's shape), followed by a user message for any text.
func expand(m core.Message) []Message {
	if m.Role == core.RoleAssistant {
		cm := Message{Role: "assistant"}
		for _, b := range m.Blocks {
			switch b.Type {
			case core.BlockText:
				cm.Content += b.Text
			case core.BlockToolUse:
				tc := ToolCall{ID: b.ID, Type: "function"}
				tc.Function.Name = b.Name
				tc.Function.Arguments = string(b.Input)
				cm.ToolCalls = append(cm.ToolCalls, tc)
			case core.BlockThinking:
				// Reasoning echoed back from a prior turn is dropped, not resent:
				// thinking-model templates expect prior reasoning stripped from
				// history (ADR-0005 point 4).
			}
		}
		if cm.Content == "" && len(cm.ToolCalls) == 0 {
			// A prior assistant turn containing only thinking blocks becomes
			// empty after reasoning-strip; drop it rather than sending
			// {"role":"assistant"}, which strict OpenAI-compat servers reject.
			return nil
		}
		return []Message{cm}
	}

	// User turn: tool results become their own messages; text becomes a user
	// message. Tool results precede the text so they follow the assistant call.
	var out []Message
	var text string
	for _, b := range m.Blocks {
		switch b.Type {
		case core.BlockText:
			text += b.Text
		case core.BlockToolResult:
			out = append(out, Message{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
		}
	}
	if text != "" || len(out) == 0 {
		out = append(out, Message{Role: "user", Content: text})
	}
	return out
}

// Tools translates core tools into the OpenAI tools array.
func Tools(tools []core.Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		var ot Tool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out = append(out, ot)
	}
	return out
}

// MapToolChoice maps Anthropic tool-choice onto the OpenAI form: a bare string
// ("auto"/"required"/"none") or a {type,function} object for a specific tool.
// nil (no constraint) leaves the field unset.
func MapToolChoice(tc *core.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case core.ToolChoiceAuto:
		return "auto"
	case core.ToolChoiceAny:
		return "required"
	case core.ToolChoiceNone:
		return "none"
	case core.ToolChoiceTool:
		return map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
	default:
		return nil
	}
}

// MapFinishReason maps OpenAI finish_reason onto Anthropic stop reasons. An
// empty/unknown reason is treated as a normal end of turn.
func MapFinishReason(reason string) core.StopReason {
	switch reason {
	case "length":
		return core.StopMaxTokens
	case "tool_calls":
		return core.StopToolUse
	case "stop", "":
		return core.StopEndTurn
	default:
		return core.StopEndTurn
	}
}

// ParseResponse maps a non-streaming engine response back to core, surfacing
// reasoning only when the client asked for thinking (ADR-0005).
func ParseResponse(req core.Request, resp Response) core.Response {
	choice := resp.Choices[0]
	blocks := make([]core.ContentBlock, 0, 2+len(choice.Message.ToolCalls))
	if EmitThinking(req) && choice.Message.ReasoningContent != "" {
		blocks = append(blocks, core.ThinkingBlock(choice.Message.ReasoningContent, ""))
	}
	if choice.Message.Content != "" {
		blocks = append(blocks, core.TextBlock(choice.Message.Content))
	}
	for i, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, core.ToolUseBlock(toolUseID(tc.ID, i), tc.Function.Name, ToolArgs(tc.Function.Arguments)))
	}

	reason := MapFinishReason(choice.FinishReason)
	if len(choice.Message.ToolCalls) > 0 {
		// A tool call always means stop_reason tool_use, even if the engine
		// reported "stop" (some do when tool_choice forced a call).
		reason = core.StopToolUse
	}
	return core.Response{
		Blocks:     blocks,
		StopReason: reason,
		Usage: core.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
}

// ToolArgs normalizes a tool call's arguments into a JSON object. Engines emit a
// JSON string like {"city":"Paris"}; an empty value becomes {}.
func ToolArgs(arguments string) json.RawMessage {
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

// toolUseID returns the engine-supplied call id, or a synthesized one when the
// engine omits it (Anthropic requires a non-empty id to pair with tool_result).
func toolUseID(id string, index int) string {
	if id != "" {
		return id
	}
	return "toolu_" + strconv.Itoa(index)
}

// EmitThinking reports whether the client asked for reasoning output.
func EmitThinking(req core.Request) bool {
	return req.Thinking != nil && req.Thinking.Enabled
}

// ThinkingKwargs maps the request's thinking setting onto the engine's chat
// template via the enable_thinking kwarg — the convention hybrid-thinking
// models (Qwen3, …) expose through --jinja templating. It is gated on the
// model's catalog reasoning capability (M2 phase 4b): a reasoning model gets
// enable_thinking set so a hybrid model defaulting to thinking-on does not emit
// reasoning the client never requested; a non-reasoning model has no thinking
// mode, so Atlas omits the kwarg entirely rather than injecting a template var
// the model does not understand. budget_tokens is advisory (ADR-0005) and not
// forwarded. It is exported so the per-engine adapters (llama.cpp, vLLM) can
// render the identical template when counting tokens.
func (c *Client) ThinkingKwargs(req core.Request) map[string]any {
	if !c.reasoning {
		return nil
	}
	return map[string]any{"enable_thinking": EmitThinking(req)}
}
