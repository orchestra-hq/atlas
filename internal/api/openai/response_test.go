package openai

import (
	"encoding/json"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

func TestFromCoreText(t *testing.T) {
	resp := core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock("hello")},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 7, OutputTokens: 3},
	}
	out := FromCore("chatcmpl-1", 1700000000, "m", resp)
	if out.Object != "chat.completion" || out.Model != "m" || out.ID != "chatcmpl-1" {
		t.Errorf("envelope = %#v", out)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d", len(out.Choices))
	}
	c := out.Choices[0]
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", c.FinishReason)
	}
	if c.Message.Content == nil || *c.Message.Content != "hello" {
		t.Errorf("content = %v", c.Message.Content)
	}
	if c.Message.Role != "assistant" {
		t.Errorf("role = %q", c.Message.Role)
	}
	if out.Usage.PromptTokens != 7 || out.Usage.CompletionTokens != 3 || out.Usage.TotalTokens != 10 {
		t.Errorf("usage = %#v", out.Usage)
	}
}

func TestFromCoreToolCallsContentNull(t *testing.T) {
	resp := core.Response{
		Blocks: []core.ContentBlock{
			core.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
		},
		StopReason: core.StopToolUse,
	}
	out := FromCore("chatcmpl-2", 1, "m", resp)
	c := out.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", c.FinishReason)
	}
	// content must serialize as JSON null on a pure tool-call turn.
	raw, _ := json.Marshal(c.Message)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if v, ok := m["content"]; !ok || v != nil {
		t.Errorf("content = %v (present=%v), want null", v, ok)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d", len(c.Message.ToolCalls))
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call = %#v", tc)
	}
	if tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
}

func TestFromCoreTextThenToolCall(t *testing.T) {
	resp := core.Response{
		Blocks: []core.ContentBlock{
			core.TextBlock("Checking."),
			core.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
		},
		StopReason: core.StopToolUse,
	}
	out := FromCore("chatcmpl-3", 1, "m", resp)
	c := out.Choices[0]
	if c.Message.Content == nil || *c.Message.Content != "Checking." {
		t.Errorf("content = %v", c.Message.Content)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Errorf("tool_calls = %d", len(c.Message.ToolCalls))
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[core.StopReason]string{
		core.StopEndTurn:      "stop",
		core.StopMaxTokens:    "length",
		core.StopToolUse:      "tool_calls",
		core.StopStopSequence: "stop",
		core.StopReason("?"):  "stop",
	}
	for in, want := range cases {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
