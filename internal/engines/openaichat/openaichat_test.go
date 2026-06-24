package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// fakeServer stands in for an engine's /v1/chat/completions endpoint.
func fakeServer(t *testing.T, status int, respBody string, capture *Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

// sseServer streams the given raw event-stream body.
func sseServer(t *testing.T, body string, capture *Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

func temp(f float64) *float64 { return &f }

// client is a reasoning-capable client (the default for the thinking tests): the
// enable_thinking kwarg is emitted, gated by request intent.
func client(srv *httptest.Server) *Client {
	return NewClient("test", srv.URL, "served-model", true, srv.Client())
}

// nonReasoningClient models a non-reasoning catalog model, which omits the
// thinking kwarg entirely (M2 phase 4b).
func nonReasoningClient(srv *httptest.Server) *Client {
	return NewClient("test", srv.URL, "served-model", false, srv.Client())
}

func TestExecuteTranslatesAndMaps(t *testing.T) {
	var got Request
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":2}
	}`, &got)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:       "served-model",
		System:      "be terse",
		MaxTokens:   32,
		Temperature: temp(0),
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if resp.Text() != "hello world" {
		t.Errorf("text = %q", resp.Text())
	}
	if resp.StopReason != core.StopEndTurn {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "be terse" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "hi" {
		t.Errorf("user message = %+v", got.Messages[1])
	}
	if got.MaxTokens != 32 || got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("max_tokens/temperature = %d / %v", got.MaxTokens, got.Temperature)
	}
	if got.Stream {
		t.Error("stream must be false")
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]core.StopReason{
		"stop":       core.StopEndTurn,
		"length":     core.StopMaxTokens,
		"tool_calls": core.StopToolUse,
		"":           core.StopEndTurn,
		"weird":      core.StopEndTurn,
	}
	for reason, want := range cases {
		if got := MapFinishReason(reason); got != want {
			t.Errorf("MapFinishReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestExecuteEmitsToolUseBlock(t *testing.T) {
	// finish_reason "stop" but a tool_call present: stop_reason must still be tool_use.
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
			"finish_reason":"stop"}],
		"usage":{"prompt_tokens":9,"completion_tokens":7}
	}`, nil)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("weather in Paris?")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.StopReason != core.StopToolUse {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != core.BlockToolUse {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	b := resp.Blocks[0]
	if b.ID != "call_1" || b.Name != "get_weather" || string(b.Input) != `{"city":"Paris"}` {
		t.Errorf("tool_use block = %+v", b)
	}
}

func TestExecuteTranslatesToolsAndLoop(t *testing.T) {
	var got Request
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"It is sunny."},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":4}
	}`, &got)
	defer srv.Close()

	_, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Tools: []core.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: &core.ToolChoice{Type: core.ToolChoiceAny},
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("weather in Paris?")}},
			{Role: core.RoleAssistant, Blocks: []core.ContentBlock{
				core.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
			}},
			{Role: core.RoleUser, Blocks: []core.ContentBlock{
				core.ToolResultBlock("call_1", "sunny, 20C", false),
			}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// tools translated to OpenAI function form, any -> required.
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if got.ToolChoice != "required" {
		t.Errorf("tool_choice = %v, want required", got.ToolChoice)
	}
	// messages: user, assistant(tool_calls), tool(result).
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	asst := got.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("assistant message = %+v", asst)
	}
	tool := got.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "sunny, 20C" {
		t.Errorf("tool message = %+v", tool)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	cases := []struct {
		in   *core.ToolChoice
		want any
	}{
		{nil, nil},
		{&core.ToolChoice{Type: core.ToolChoiceAuto}, "auto"},
		{&core.ToolChoice{Type: core.ToolChoiceAny}, "required"},
		{&core.ToolChoice{Type: core.ToolChoiceNone}, "none"},
	}
	for _, c := range cases {
		if got := MapToolChoice(c.in); got != c.want {
			t.Errorf("MapToolChoice(%+v) = %v, want %v", c.in, got, c.want)
		}
	}
	// Specific tool produces a function-selector object.
	obj, ok := MapToolChoice(&core.ToolChoice{Type: core.ToolChoiceTool, Name: "get_weather"}).(map[string]any)
	if !ok || obj["type"] != "function" {
		t.Fatalf("specific tool choice = %#v", obj)
	}
}

func TestExecuteEmitsThinkingBlock(t *testing.T) {
	var got Request
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","reasoning_content":"let me think","content":"the answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":6}
	}`, &got)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Thinking:  &core.ThinkingConfig{Enabled: true, BudgetTokens: 1024},
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Type != core.BlockThinking || resp.Blocks[1].Type != core.BlockText {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[0].Thinking != "let me think" {
		t.Errorf("thinking = %q", resp.Blocks[0].Thinking)
	}
	if got.ChatTemplateKwargs["enable_thinking"] != true {
		t.Errorf("enable_thinking = %v, want true", got.ChatTemplateKwargs["enable_thinking"])
	}
}

// vLLM (0.23.0) returns the reasoning trace in `reasoning`, not the
// `reasoning_content` llama.cpp/SGLang use. Atlas must surface either as a
// thinking block (otherwise the trace is dropped and only the answer shows —
// the real G4 failure on vLLM).
func TestExecuteEmitsThinkingBlockFromReasoningField(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","reasoning":"let me think","content":"the answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":6}
	}`, nil)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Thinking:  &core.ThinkingConfig{Enabled: true, BudgetTokens: 1024},
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Type != core.BlockThinking || resp.Blocks[1].Type != core.BlockText {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[0].Thinking != "let me think" {
		t.Errorf("thinking = %q", resp.Blocks[0].Thinking)
	}
}

func TestExecuteDropsReasoningWhenThinkingOff(t *testing.T) {
	var got Request
	// Engine still returned reasoning_content, but the client did not ask for it.
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","reasoning_content":"stray","content":"answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`, &got)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != core.BlockText {
		t.Fatalf("blocks = %+v, want a single text block", resp.Blocks)
	}
	if got.ChatTemplateKwargs["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", got.ChatTemplateKwargs["enable_thinking"])
	}
}

// TestNonReasoningModelOmitsThinkingKwarg: a non-reasoning catalog model never
// sends enable_thinking — the kwarg is absent from chat_template_kwargs (and the
// map is nil/empty), even when the client requests thinking (M2 phase 4b).
func TestNonReasoningModelOmitsThinkingKwarg(t *testing.T) {
	var got Request
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`, &got)
	defer srv.Close()

	if _, err := nonReasoningClient(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Thinking:  &core.ThinkingConfig{Enabled: true}, // client asks, but the model can't reason
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, present := got.ChatTemplateKwargs["enable_thinking"]; present {
		t.Errorf("enable_thinking present for a non-reasoning model: %v", got.ChatTemplateKwargs)
	}
}

func TestAssistantThinkingOnlyTurnIsDropped(t *testing.T) {
	var got Request
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`, &got)
	defer srv.Close()

	_, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Thinking:  &core.ThinkingConfig{Enabled: true},
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("first")}},
			// Echoed prior assistant turn containing only reasoning should not
			// produce an empty assistant chat message after reasoning-strip.
			{Role: core.RoleAssistant, Blocks: []core.ContentBlock{core.ThinkingBlock("scratchpad", "")}},
			{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("second")}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %+v, want user/user (thinking-only assistant dropped)", got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[1].Role != "user" {
		t.Errorf("roles = %q, %q", got.Messages[0].Role, got.Messages[1].Role)
	}
}

func TestExecuteEngineErrorStatus(t *testing.T) {
	srv := fakeServer(t, http.StatusServiceUnavailable, `{"error":"loading model"}`, nil)
	defer srv.Close()

	_, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if !errors.Is(err, core.ErrEngineUnavailable) {
		t.Errorf("err = %v, want wrapped ErrEngineUnavailable", err)
	}
}

func TestExecuteNoChoices(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, `{"choices":[],"usage":{}}`, nil)
	defer srv.Close()

	_, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error when no choices returned")
	}
}

// recordSink captures the deltas, tool-call events, and terminal signal a
// stream produces.
type recordSink struct {
	thinking   string // concatenated thinking deltas
	deltas     []string
	toolStarts []string // "name" per ToolCallStart, in order
	toolArgs   string   // concatenated tool-call argument fragments
	reason     core.StopReason
	usage      core.Usage
	done       bool
	stopAfter  int // return ErrStopStreaming after this many deltas (0 = never)
}

func (s *recordSink) Thinking(delta string) error {
	s.thinking += delta
	return nil
}

func (s *recordSink) Text(delta string) error {
	s.deltas = append(s.deltas, delta)
	if s.stopAfter > 0 && len(s.deltas) >= s.stopAfter {
		return core.ErrStopStreaming
	}
	return nil
}

func (s *recordSink) ToolCallStart(_ int, _, name string) error {
	s.toolStarts = append(s.toolStarts, name)
	return nil
}

func (s *recordSink) ToolCallDelta(_ int, argsFragment string) error {
	s.toolArgs += argsFragment
	return nil
}

func (s *recordSink) Done(reason core.StopReason, usage core.Usage) error {
	s.done = true
	s.reason = reason
	s.usage = usage
	return nil
}

func TestExecuteStreamForwardsThinking(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hmm \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, body, nil)
	defer srv.Close()

	sink := &recordSink{}
	if err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 16,
		Thinking:  &core.ThinkingConfig{Enabled: true},
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	}, sink); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if sink.thinking != "hmm ok" {
		t.Errorf("thinking = %q", sink.thinking)
	}
	if len(sink.deltas) != 1 || sink.deltas[0] != "done" {
		t.Errorf("text deltas = %v", sink.deltas)
	}
}

// vLLM streams thinking deltas under `reasoning`, not `reasoning_content`.
func TestExecuteStreamForwardsThinkingFromReasoningField(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"hmm \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, body, nil)
	defer srv.Close()

	sink := &recordSink{}
	if err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 16,
		Thinking:  &core.ThinkingConfig{Enabled: true},
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("q")}}},
	}, sink); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if sink.thinking != "hmm ok" {
		t.Errorf("thinking = %q", sink.thinking)
	}
	if len(sink.deltas) != 1 || sink.deltas[0] != "done" {
		t.Errorf("text deltas = %v", sink.deltas)
	}
}

func TestExecuteStreamForwardsDeltasAndUsage(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var got Request
	srv := sseServer(t, body, &got)
	defer srv.Close()

	sink := &recordSink{}
	err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 16,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	}, sink)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if !got.Stream || got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Errorf("stream/stream_options not set: %+v", got)
	}
	if joined := sink.deltas; len(joined) != 2 || joined[0] != "Hel" || joined[1] != "lo" {
		t.Errorf("deltas = %v", joined)
	}
	if !sink.done || sink.reason != core.StopEndTurn {
		t.Errorf("done=%v reason=%q", sink.done, sink.reason)
	}
	if sink.usage.InputTokens != 5 || sink.usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", sink.usage)
	}
}

func TestExecuteStreamStopSignalEndsCleanly(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"one\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"three\"},\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, body, nil)
	defer srv.Close()

	sink := &recordSink{stopAfter: 1}
	if err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 16,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	}, sink); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	// Stopped after the first delta; Done is not called (the gateway finalizes).
	if len(sink.deltas) != 1 || sink.done {
		t.Errorf("deltas=%v done=%v, want 1 delta and no Done", sink.deltas, sink.done)
	}
}

func TestExecuteStreamToolCall(t *testing.T) {
	// id+name arrive first, then argument fragments across chunks, then finish.
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, body, nil)
	defer srv.Close()

	sink := &recordSink{}
	if err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("weather?")}}},
	}, sink); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if len(sink.toolStarts) != 1 || sink.toolStarts[0] != "get_weather" {
		t.Errorf("tool starts = %v", sink.toolStarts)
	}
	if sink.toolArgs != `{"city":"Paris"}` {
		t.Errorf("tool args = %q", sink.toolArgs)
	}
	if !sink.done || sink.reason != core.StopToolUse {
		t.Errorf("done=%v reason=%q, want tool_use", sink.done, sink.reason)
	}
}

func TestExecuteStreamErrorStatus(t *testing.T) {
	srv := fakeServer(t, http.StatusServiceUnavailable, `{"error":"loading"}`, nil)
	defer srv.Close()

	err := client(srv).ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	}, &recordSink{})
	if !errors.Is(err, core.ErrEngineUnavailable) {
		t.Errorf("err = %v, want wrapped ErrEngineUnavailable", err)
	}
}

// TestToolUseIDSynthesized proves an omitted engine call id is synthesized so
// the Anthropic surface always has a non-empty id to pair with tool_result.
func TestToolUseIDSynthesized(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"","type":"function","function":{"name":"f","arguments":""}}]},
			"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`, nil)
	defer srv.Close()

	resp, err := client(srv).Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].ID != "toolu_0" {
		t.Errorf("synthesized id = %+v", resp.Blocks)
	}
	// Empty arguments normalize to an object.
	if string(resp.Blocks[0].Input) != "{}" {
		t.Errorf("args = %q, want {}", resp.Blocks[0].Input)
	}
}
