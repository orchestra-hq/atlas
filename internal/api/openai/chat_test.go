package openai

import (
	"encoding/json"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

func mustToCore(t *testing.T, body string) core.Request {
	t.Helper()
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cr, err := req.ToCore()
	if err != nil {
		t.Fatalf("ToCore: %v", err)
	}
	return cr
}

func TestToCoreBasic(t *testing.T) {
	cr := mustToCore(t, `{
		"model":"m","max_tokens":64,"temperature":0,
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hi"}
		]
	}`)
	if cr.Model != "m" || cr.MaxTokens != 64 {
		t.Errorf("model/max = %q/%d", cr.Model, cr.MaxTokens)
	}
	if cr.System != "be terse" {
		t.Errorf("system = %q", cr.System)
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Role != core.RoleUser || cr.Messages[0].Text() != "hi" {
		t.Errorf("messages = %#v", cr.Messages)
	}
	if cr.Temperature == nil || *cr.Temperature != 0 {
		t.Errorf("temperature = %v", cr.Temperature)
	}
}

func TestToCoreMaxCompletionTokensAlias(t *testing.T) {
	cr := mustToCore(t, `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if cr.MaxTokens != 32 {
		t.Errorf("max = %d, want 32", cr.MaxTokens)
	}
}

func TestToCoreDefaultMaxTokens(t *testing.T) {
	cr := mustToCore(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if cr.MaxTokens != defaultMaxTokens {
		t.Errorf("max = %d, want default %d", cr.MaxTokens, defaultMaxTokens)
	}
}

func TestToCoreContentParts(t *testing.T) {
	cr := mustToCore(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"text","text":"a "},{"type":"text","text":"b"}]}
	]}`)
	if got := cr.Messages[0].Text(); got != "a b" {
		t.Errorf("text = %q", got)
	}
}

func TestToCoreStopStringAndArray(t *testing.T) {
	one := mustToCore(t, `{"model":"m","max_tokens":8,"stop":"END","messages":[{"role":"user","content":"x"}]}`)
	if len(one.StopSequences) != 1 || one.StopSequences[0] != "END" {
		t.Errorf("stop = %v", one.StopSequences)
	}
	many := mustToCore(t, `{"model":"m","max_tokens":8,"stop":["A","B"],"messages":[{"role":"user","content":"x"}]}`)
	if len(many.StopSequences) != 2 || many.StopSequences[1] != "B" {
		t.Errorf("stop = %v", many.StopSequences)
	}
}

func TestToCoreTools(t *testing.T) {
	cr := mustToCore(t, `{
		"model":"m","max_tokens":8,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":"required",
		"messages":[{"role":"user","content":"x"}]
	}`)
	if len(cr.Tools) != 1 || cr.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %#v", cr.Tools)
	}
	if string(cr.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("schema = %s", cr.Tools[0].InputSchema)
	}
	if cr.ToolChoice == nil || cr.ToolChoice.Type != core.ToolChoiceAny {
		t.Errorf("tool_choice = %#v", cr.ToolChoice)
	}
}

func TestToCoreToolChoiceSpecific(t *testing.T) {
	cr := mustToCore(t, `{
		"model":"m","max_tokens":8,
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"messages":[{"role":"user","content":"x"}]
	}`)
	if cr.ToolChoice == nil || cr.ToolChoice.Type != core.ToolChoiceTool || cr.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice = %#v", cr.ToolChoice)
	}
}

func TestToCoreToolRoundTripMessages(t *testing.T) {
	// assistant tool_calls then a tool result, the OpenAI multi-turn tool shape.
	cr := mustToCore(t, `{
		"model":"m","max_tokens":64,
		"messages":[
			{"role":"user","content":"weather in Paris?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"15C sunny"}
		]
	}`)
	if len(cr.Messages) != 3 {
		t.Fatalf("messages = %d", len(cr.Messages))
	}
	asst := cr.Messages[1]
	if asst.Role != core.RoleAssistant || len(asst.Blocks) != 1 || asst.Blocks[0].Type != core.BlockToolUse {
		t.Fatalf("assistant = %#v", asst)
	}
	if asst.Blocks[0].ID != "call_1" || asst.Blocks[0].Name != "get_weather" {
		t.Errorf("tool_use = %#v", asst.Blocks[0])
	}
	tool := cr.Messages[2]
	if tool.Role != core.RoleUser || tool.Blocks[0].Type != core.BlockToolResult || tool.Blocks[0].ToolUseID != "call_1" {
		t.Errorf("tool result = %#v", tool)
	}
	if tool.Blocks[0].Content != "15C sunny" {
		t.Errorf("tool content = %q", tool.Blocks[0].Content)
	}
}

func TestToCoreValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing model":        `{"max_tokens":8,"messages":[{"role":"user","content":"x"}]}`,
		"empty messages":       `{"model":"m","max_tokens":8,"messages":[]}`,
		"bad role":             `{"model":"m","max_tokens":8,"messages":[{"role":"captain","content":"x"}]}`,
		"tool without id":      `{"model":"m","max_tokens":8,"messages":[{"role":"tool","content":"x"}]}`,
		"tool_call without id": `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f"}}]}]}`,
		"bad tool_choice":      `{"model":"m","max_tokens":8,"tool_choice":"banana","messages":[{"role":"user","content":"x"}]}`,
		"tool without name":    `{"model":"m","max_tokens":8,"tools":[{"type":"function","function":{}}],"messages":[{"role":"user","content":"x"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var req ChatCompletionRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, err := req.ToCore()
			if err == nil {
				t.Fatalf("expected error")
			}
			var apiErr *Error
			if !asErr(err, &apiErr) {
				t.Fatalf("error type = %T", err)
			}
			if apiErr.Status != 400 {
				t.Errorf("status = %d", apiErr.Status)
			}
		})
	}
}

// asErr is a tiny errors.As wrapper that avoids importing errors in the table.
func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
