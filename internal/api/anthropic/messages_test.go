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
