package anthropic

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

func TestStringOrBlocksUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare string", `"hello"`, "hello"},
		{"block list", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"null", `null`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s StringOrBlocks
			if err := json.Unmarshal([]byte(tc.in), &s); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if s.Text() != tc.want {
				t.Errorf("Text() = %q, want %q", s.Text(), tc.want)
			}
		})
	}
}

func TestStringOrBlocksRejectsScalar(t *testing.T) {
	var s StringOrBlocks
	if err := json.Unmarshal([]byte(`42`), &s); err == nil {
		t.Fatal("expected error for numeric content")
	}
}

func TestToCoreValid(t *testing.T) {
	var req MessagesRequest
	body := `{
		"model": "m",
		"system": "be brief",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type":"text","text":"hello"}]},
			{"role": "user", "content": "bye"}
		]
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := req.ToCore()
	if err != nil {
		t.Fatalf("ToCore: %v", err)
	}
	if got.System != "be brief" {
		t.Errorf("system = %q", got.System)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(got.Messages))
	}
	if got.Messages[0].Role != core.RoleUser || got.Messages[0].Text() != "hi" {
		t.Errorf("messages[0] = %+v", got.Messages[0])
	}
	if got.Messages[1].Role != core.RoleAssistant || got.Messages[1].Text() != "hello" {
		t.Errorf("messages[1] = %+v", got.Messages[1])
	}
}

func TestToCoreErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing model", `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`},
		{"zero max_tokens", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`},
		{"missing max_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`},
		{"empty messages", `{"model":"m","max_tokens":1,"messages":[]}`},
		{"bad role", `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":"x"}]}`},
		{"non-text block", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image"}]}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req MessagesRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, err := req.ToCore()
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Type != ErrInvalidRequest {
				t.Errorf("err = %v, want invalid_request_error", err)
			}
		})
	}
}

func TestToCoreTools(t *testing.T) {
	body := `{
		"model": "m",
		"max_tokens": 64,
		"tools": [{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice": {"type":"tool","name":"get_weather"},
		"messages": [
			{"role":"user","content":"weather in Paris?"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny","is_error":false}]}
		]
	}`
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := req.ToCore()
	if err != nil {
		t.Fatalf("ToCore: %v", err)
	}

	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if string(got.Tools[0].InputSchema) == "" {
		t.Error("input_schema not carried")
	}
	if got.ToolChoice == nil || got.ToolChoice.Type != core.ToolChoiceTool || got.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice = %+v", got.ToolChoice)
	}

	use := got.Messages[1].Blocks[0]
	if use.Type != core.BlockToolUse || use.ID != "call_1" || use.Name != "get_weather" || string(use.Input) != `{"city":"Paris"}` {
		t.Errorf("tool_use block = %+v", use)
	}
	res := got.Messages[2].Blocks[0]
	if res.Type != core.BlockToolResult || res.ToolUseID != "call_1" || res.Content != "sunny" {
		t.Errorf("tool_result block = %+v", res)
	}
}

func TestToCoreToolChoiceErrors(t *testing.T) {
	tests := []string{
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"bogus"}}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"tool"}}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"description":"no name"}]}`,
	}
	for _, body := range tests {
		var req MessagesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, err := req.ToCore(); err == nil {
			t.Errorf("expected error for %s", body)
		}
	}
}

func TestFromCoreToolUse(t *testing.T) {
	resp := core.Response{
		Blocks: []core.ContentBlock{
			core.TextBlock("Let me check."),
			core.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
		},
		StopReason: core.StopToolUse,
		Usage:      core.Usage{InputTokens: 5, OutputTokens: 9},
	}
	wire := FromCore("msg_1", "m", resp, nil)
	if wire.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", wire.StopReason)
	}
	if len(wire.Content) != 2 {
		t.Fatalf("content = %+v", wire.Content)
	}

	// Marshal and re-parse to assert the exact wire shape of each block.
	raw, err := json.Marshal(wire.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "Let me check." {
		t.Errorf("text block = %v", blocks[0])
	}
	if _, ok := blocks[0]["input"]; ok {
		t.Error("text block leaked an input field")
	}
	if blocks[1]["type"] != "tool_use" || blocks[1]["id"] != "call_1" || blocks[1]["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", blocks[1])
	}
	input, ok := blocks[1]["input"].(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Errorf("tool_use input = %v", blocks[1]["input"])
	}
}

func TestFromCore(t *testing.T) {
	seq := "stopword"
	resp := core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock("done")},
		StopReason: core.StopStopSequence,
		Usage:      core.Usage{InputTokens: 3, OutputTokens: 1},
	}
	wire := FromCore("msg_1", "m", resp, &seq)
	if wire.Type != "message" || wire.Role != "assistant" || wire.Model != "m" {
		t.Errorf("envelope = %+v", wire)
	}
	if len(wire.Content) != 1 || wire.Content[0].Text != "done" {
		t.Errorf("content = %+v", wire.Content)
	}
	if wire.StopReason != "stop_sequence" || wire.StopSequence == nil || *wire.StopSequence != "stopword" {
		t.Errorf("stop = %q / %v", wire.StopReason, wire.StopSequence)
	}
	if wire.Usage.InputTokens != 3 || wire.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", wire.Usage)
	}
}
