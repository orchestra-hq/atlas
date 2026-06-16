// Package llamacpp adapts llama.cpp's bundled HTTP server (llama-server) to
// Atlas's engine interface. Per build-time decision 1 in
// docs/m0-build-plan.md, the adapter speaks llama-server's OpenAI-compatible
// /v1/chat/completions endpoint; the gateway produces all Anthropic
// semantics, so one conformance result holds across engines.
package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/orchestra-hq/atlas/internal/core"
)

// Adapter executes core requests against a running llama-server instance.
type Adapter struct {
	baseURL string
	model   string
	client  *http.Client
}

// New builds an adapter targeting a llama-server at baseURL (e.g.
// http://127.0.0.1:8080). model is the name echoed in the OpenAI payload;
// llama-server serves whatever weights it was launched with regardless.
func New(baseURL, model string, client *http.Client) *Adapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &Adapter{baseURL: baseURL, model: model, client: client}
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is the message text. Omitted on an assistant turn that only
	// carries tool calls (OpenAI accepts a missing/empty content there).
	Content string `json:"content,omitempty"`
	// ToolCalls is set on an assistant turn that called tools.
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a role:"tool" message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// toolCall is one OpenAI tool call (request and response shape coincide).
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiTool is one entry of the OpenAI tools array.
type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	TopK        *int          `json:"top_k,omitempty"` // llama.cpp extension, ignored by strict OpenAI servers
	Tools       []oaiTool     `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"` // string or {type,function} object
	// ChatTemplateKwargs are passed to the engine's Jinja chat template. Atlas
	// uses it to toggle reasoning on hybrid-thinking models (ADR-0005); see
	// thinkingKwargs.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	Stream             bool           `json:"stream"`
	StreamOptions      *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions asks the OpenAI-compat server to include a final usage chunk
// when streaming (llama-server honors this).
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// ReasoningContent is the model's reasoning trace, separated by
			// llama-server's reasoning parser (the de-facto field name shared by
			// llama.cpp, vLLM, and SGLang). Empty for non-reasoning models.
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Execute translates req to OpenAI chat form, calls llama-server, and maps the
// result back to core. Stop sequences are intentionally not forwarded: the
// gateway owns that semantics (see server.Gateway).
func (a *Adapter) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	body, err := json.Marshal(a.toChat(req, false))
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: call engine: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return core.Response{}, fmt.Errorf("llamacpp: engine returned %d: %s", resp.StatusCode, truncate(raw, 512))
	}

	var chat chatResponse
	if err := json.Unmarshal(raw, &chat); err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: decode response: %w", err)
	}
	if len(chat.Choices) == 0 {
		return core.Response{}, fmt.Errorf("llamacpp: engine returned no choices")
	}

	choice := chat.Choices[0]
	blocks := make([]core.ContentBlock, 0, 2+len(choice.Message.ToolCalls))
	// Reasoning precedes the answer. Only surface it when the client asked for
	// thinking (ADR-0005); otherwise a hybrid model's stray reasoning is dropped.
	if emitThinking(req) && choice.Message.ReasoningContent != "" {
		blocks = append(blocks, core.ThinkingBlock(choice.Message.ReasoningContent, ""))
	}
	if choice.Message.Content != "" {
		blocks = append(blocks, core.TextBlock(choice.Message.Content))
	}
	for i, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, core.ToolUseBlock(toolUseID(tc.ID, i), tc.Function.Name, toolArgs(tc.Function.Arguments)))
	}

	reason := mapFinishReason(choice.FinishReason)
	if len(choice.Message.ToolCalls) > 0 {
		// A tool call always means stop_reason tool_use, even if the engine
		// reported "stop" (some do when tool_choice forced a call).
		reason = core.StopToolUse
	}

	return core.Response{
		Blocks:     blocks,
		StopReason: reason,
		Usage: core.Usage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
		},
	}, nil
}

// toolArgs normalizes a tool call's arguments into a JSON object. llama-server
// emits a JSON string like {"city":"Paris"}; an empty value becomes {}.
func toolArgs(arguments string) json.RawMessage {
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
	return fmt.Sprintf("toolu_%d", index)
}

// chatStreamChunk is one Server-Sent Event from the OpenAI-compat streaming
// endpoint. Deltas carry incremental content; the terminal chunk carries a
// finish_reason and (with stream_options.include_usage) a usage object.
type chatStreamChunk struct {
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

// ExecuteStream runs req against llama-server with stream=true, forwarding each
// content delta to sink.Text and ending with sink.Done. If a sink method
// returns core.ErrStopStreaming (the gateway's signal that a stop sequence
// matched), generation is abandoned and ExecuteStream returns nil — the stream
// is finalized by the gateway, not here. As with Execute, stop sequences are
// owned by the gateway and never forwarded to the engine.
func (a *Adapter) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	body, err := json.Marshal(a.toChat(req, true))
	if err != nil {
		return fmt.Errorf("llamacpp: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llamacpp: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("llamacpp: call engine: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("llamacpp: engine returned %d: %s", resp.StatusCode, truncate(raw, 512))
	}

	reason := core.StopEndTurn
	var usage core.Usage
	sawToolCall := false
	wantThinking := emitThinking(req)
	started := map[int]bool{} // tool-call indices already announced via ToolCallStart
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // event:/id:/comment/blank lines
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("llamacpp: decode stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			usage = core.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue // usage-only final chunk
		}
		choice := chunk.Choices[0]
		if wantThinking && choice.Delta.ReasoningContent != "" {
			if stop, err := pump(sink.Thinking(choice.Delta.ReasoningContent)); err != nil {
				return err
			} else if stop {
				return nil
			}
		}
		if choice.Delta.Content != "" {
			if stop, err := pump(sink.Text(choice.Delta.Content)); err != nil {
				return err
			} else if stop {
				return nil
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			sawToolCall = true
			if !started[tc.Index] {
				started[tc.Index] = true
				if stop, err := pump(sink.ToolCallStart(tc.Index, toolUseID(tc.ID, tc.Index), tc.Function.Name)); err != nil {
					return err
				} else if stop {
					return nil
				}
			}
			if tc.Function.Arguments != "" {
				if stop, err := pump(sink.ToolCallDelta(tc.Index, tc.Function.Arguments)); err != nil {
					return err
				} else if stop {
					return nil
				}
			}
		}
		if choice.FinishReason != nil {
			reason = mapFinishReason(*choice.FinishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("llamacpp: read stream: %w", err)
	}
	if sawToolCall {
		reason = core.StopToolUse
	}

	if _, err := pump(sink.Done(reason, usage)); err != nil {
		return err
	}
	return nil
}

// pump interprets the error from a StreamSink call: ErrStopStreaming means the
// gateway wants generation to end cleanly (stop=true, no error); any other
// non-nil error propagates.
func pump(err error) (stop bool, _ error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, core.ErrStopStreaming):
		return true, nil
	default:
		return false, err
	}
}

func (a *Adapter) toChat(req core.Request, stream bool) chatRequest {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, toChatMessages(m)...)
	}
	cr := chatRequest{
		Model:              a.model,
		Messages:           msgs,
		MaxTokens:          req.MaxTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		TopK:               req.TopK,
		Tools:              toChatTools(req.Tools),
		ToolChoice:         toChatToolChoice(req.ToolChoice),
		ChatTemplateKwargs: thinkingKwargs(req),
		Stream:             stream,
	}
	if stream {
		cr.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return cr
}

// emitThinking reports whether the client asked for reasoning output.
func emitThinking(req core.Request) bool {
	return req.Thinking != nil && req.Thinking.Enabled
}

// thinkingKwargs maps the request's thinking setting onto the engine's chat
// template via the enable_thinking kwarg — the convention hybrid-thinking
// models (Qwen3, …) expose through llama-server's --jinja templating. Atlas
// always sets it so a hybrid model defaulting to thinking-on does not emit
// reasoning the client never requested; on a non-reasoning model the unused
// kwarg is harmless. budget_tokens is advisory (ADR-0005) and not forwarded:
// llama.cpp has no reasoning-budget knob, so it is accepted and ignored.
func thinkingKwargs(req core.Request) map[string]any {
	return map[string]any{"enable_thinking": emitThinking(req)}
}

// toChatMessages expands one core message into OpenAI chat messages. Most
// messages map one-to-one, but a user turn carrying tool_result blocks becomes
// a separate role:"tool" message per result (OpenAI's shape), followed by a
// user message for any accompanying text.
func toChatMessages(m core.Message) []chatMessage {
	if m.Role == core.RoleAssistant {
		cm := chatMessage{Role: "assistant"}
		for _, b := range m.Blocks {
			switch b.Type {
			case core.BlockText:
				cm.Content += b.Text
			case core.BlockToolUse:
				tc := toolCall{ID: b.ID, Type: "function"}
				tc.Function.Name = b.Name
				tc.Function.Arguments = string(b.Input)
				cm.ToolCalls = append(cm.ToolCalls, tc)
			case core.BlockThinking:
				// Reasoning echoed back from a prior turn is dropped, not resent:
				// thinking-model templates expect prior reasoning stripped from
				// history (ADR-0005 point 4).
			}
		}
		return []chatMessage{cm}
	}

	// User turn: tool results become their own messages; text becomes a user
	// message. Tool results precede the text so they follow the assistant call.
	var out []chatMessage
	var text string
	for _, b := range m.Blocks {
		switch b.Type {
		case core.BlockText:
			text += b.Text
		case core.BlockToolResult:
			out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
		}
	}
	if text != "" || len(out) == 0 {
		out = append(out, chatMessage{Role: "user", Content: text})
	}
	return out
}

// toChatTools translates core tools into the OpenAI tools array.
func toChatTools(tools []core.Tool) []oaiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]oaiTool, 0, len(tools))
	for _, t := range tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out = append(out, ot)
	}
	return out
}

// toChatToolChoice maps Anthropic tool-choice onto the OpenAI form: a bare
// string ("auto"/"required"/"none") or a {type,function} object for a specific
// tool. nil (no constraint) leaves the field unset.
func toChatToolChoice(tc *core.ToolChoice) any {
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
		choice := map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		return choice
	default:
		return nil
	}
}

// mapFinishReason maps OpenAI finish_reason onto Anthropic stop reasons. An
// empty/unknown reason is treated as a normal end of turn.
func mapFinishReason(reason string) core.StopReason {
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

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
