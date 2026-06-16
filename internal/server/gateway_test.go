package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

const (
	testKey   = "test-key"
	testModel = "test-model"
)

// echoExecutor replies with a fixed text, echoing the request so tests can
// assert the gateway stripped stop sequences before dispatch.
type echoExecutor struct {
	reply    string
	gotReq   core.Request
	err      error
	outToken int
}

func (e *echoExecutor) Execute(_ context.Context, req core.Request) (core.Response, error) {
	e.gotReq = req
	if e.err != nil {
		return core.Response{}, e.err
	}
	out := e.outToken
	if out == 0 {
		out = 5
	}
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock(e.reply)},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 7, OutputTokens: out},
	}, nil
}

func newTestServer(exec Executor) *httptest.Server {
	g := NewGateway(testKey, map[string]Executor{testModel: exec})
	return httptest.NewServer(g.Handler())
}

func post(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

func TestMessagesHappyPath(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "hello there"})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != testModel {
		t.Errorf("envelope = %v", body)
	}
	if body["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	first := content[0].(map[string]any)
	if first["text"] != "hello there" {
		t.Errorf("text = %v", first["text"])
	}
}

func TestStopSequenceAppliedByGateway(t *testing.T) {
	exec := &echoExecutor{reply: "one two three four"}
	srv := newTestServer(exec)
	defer srv.Close()

	resp, body := post(t, srv, testKey,
		`{"model":"test-model","max_tokens":16,"stop_sequences":["three"],"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body["stop_reason"] != "stop_sequence" || body["stop_sequence"] != "three" {
		t.Errorf("stop = %v / %v", body["stop_reason"], body["stop_sequence"])
	}
	content := body["content"].([]any)
	if got := content[0].(map[string]any)["text"]; got != "one two " {
		t.Errorf("text = %q", got)
	}
	// The gateway must not leak stop sequences to the engine.
	if exec.gotReq.StopSequences != nil {
		t.Errorf("executor saw stop sequences: %v", exec.gotReq.StopSequences)
	}
}

func TestAuthErrors(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	body := `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	for _, key := range []string{"", "wrong-key"} {
		resp, parsed := post(t, srv, key, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
		if errType(parsed) != "authentication_error" {
			t.Errorf("key %q: type = %v", key, errType(parsed))
		}
	}
}

func TestBearerAuthAccepted(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownModel404(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"nope","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound || errType(body) != "not_found_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

// streamExecutor emits a fixed sequence of deltas natively (implements
// StreamExecutor). A stop sequence may end the stream early via the sink.
type streamExecutor struct {
	deltas []string
	err    error
}

func (e *streamExecutor) Execute(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{Blocks: []core.ContentBlock{core.TextBlock(strings.Join(e.deltas, ""))}, StopReason: core.StopEndTurn}, nil
}

func (e *streamExecutor) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	if e.err != nil {
		return e.err
	}
	for _, d := range e.deltas {
		if err := sink.Text(d); err != nil {
			if err == core.ErrStopStreaming {
				return nil
			}
			return err
		}
	}
	return sink.Done(core.StopEndTurn, core.Usage{InputTokens: 4, OutputTokens: len(e.deltas)})
}

// streamPost issues a streaming POST and returns the parsed SSE events.
func streamPost(t *testing.T, srv *httptest.Server, body string) (*http.Response, []sseEvent) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return resp, parseSSEEvents(t, string(raw))
}

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("event %q bad data %q: %v", name, data, err)
		}
		events = append(events, sseEvent{name: name, data: obj})
	}
	return events
}

func eventNames(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.name != "ping" {
			out = append(out, e.name)
		}
	}
	return out
}

func streamText(events []sseEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.name == "content_block_delta" {
			b.WriteString(e.data["delta"].(map[string]any)["text"].(string))
		}
	}
	return b.String()
}

func TestStreamNativeSequence(t *testing.T) {
	srv := newTestServer(&streamExecutor{deltas: []string{"stream ", "me ", "please"}})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	names := eventNames(events)
	if names[0] != "message_start" || names[1] != "content_block_start" {
		t.Errorf("prefix = %v", names[:2])
	}
	last3 := names[len(names)-3:]
	if strings.Join(last3, ",") != "content_block_stop,message_delta,message_stop" {
		t.Errorf("suffix = %v", last3)
	}
	if streamText(events) != "stream me please" {
		t.Errorf("text = %q", streamText(events))
	}

	// message_delta carries the stop reason.
	for _, e := range events {
		if e.name == "message_delta" {
			if e.data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
				t.Errorf("stop_reason = %v", e.data["delta"])
			}
		}
	}
}

func TestStreamBufferedFallback(t *testing.T) {
	// echoExecutor implements only Executor; the gateway must still stream it.
	srv := newTestServer(&echoExecutor{reply: "buffered reply"})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if streamText(events) != "buffered reply" {
		t.Errorf("text = %q", streamText(events))
	}
	if names := eventNames(events); names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Errorf("names = %v", names)
	}
}

func TestStreamStopSequence(t *testing.T) {
	srv := newTestServer(&streamExecutor{deltas: []string{"keep ", "this STOP", " drop"}})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"stop_sequences":["STOP"],"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := streamText(events); got != "keep this " {
		t.Errorf("text = %q, want %q", got, "keep this ")
	}
	for _, e := range events {
		if e.name == "message_delta" {
			delta := e.data["delta"].(map[string]any)
			if delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "STOP" {
				t.Errorf("delta = %v", delta)
			}
		}
	}
}

func TestStreamEngineErrorEmitsErrorEvent(t *testing.T) {
	srv := newTestServer(&streamExecutor{err: io.ErrUnexpectedEOF})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (headers already sent → 200)", resp.StatusCode)
	}
	var sawError bool
	for _, e := range events {
		if e.name == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("expected an error event, got %v", eventNames(events))
	}
}

// toolExecutor streams a short preamble then a single tool call, and returns
// the same content from a non-streaming Execute.
type toolExecutor struct{}

func (toolExecutor) Execute(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{
		Blocks: []core.ContentBlock{
			core.TextBlock("Checking."),
			core.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
		},
		StopReason: core.StopToolUse,
		Usage:      core.Usage{InputTokens: 5, OutputTokens: 9},
	}, nil
}

func (toolExecutor) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	if err := sink.Text("Checking."); err != nil {
		return err
	}
	if err := sink.ToolCallStart(0, "call_1", "get_weather"); err != nil {
		return err
	}
	if err := sink.ToolCallDelta(0, `{"city":`); err != nil {
		return err
	}
	if err := sink.ToolCallDelta(0, `"Paris"}`); err != nil {
		return err
	}
	return sink.Done(core.StopToolUse, core.Usage{InputTokens: 5, OutputTokens: 9})
}

// toolArgsFromEvents concatenates input_json_delta fragments per block index.
func toolArgsFromEvents(events []sseEvent) map[float64]string {
	args := map[float64]string{}
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		if d["type"] == "input_json_delta" {
			idx := e.data["index"].(float64)
			args[idx] += d["partial_json"].(string)
		}
	}
	return args
}

func TestStreamToolUse(t *testing.T) {
	srv := newTestServer(toolExecutor{})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"tool_choice":{"type":"any"},`+
			`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],`+
			`"messages":[{"role":"user","content":"weather in Paris?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Two content blocks: text at index 0, tool_use at index 1.
	starts := map[float64]string{}
	for _, e := range events {
		if e.name == "content_block_start" {
			starts[e.data["index"].(float64)] = e.data["content_block"].(map[string]any)["type"].(string)
		}
	}
	if starts[0] != "text" || starts[1] != "tool_use" {
		t.Errorf("block types = %v", starts)
	}

	args := toolArgsFromEvents(events)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args[1]), &parsed); err != nil {
		t.Fatalf("tool args %q invalid: %v", args[1], err)
	}
	if parsed["city"] != "Paris" {
		t.Errorf("args = %v", parsed)
	}

	for _, e := range events {
		if e.name == "message_delta" && e.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
			t.Errorf("stop_reason = %v", e.data["delta"])
		}
	}
}

func TestNonStreamingToolUse(t *testing.T) {
	srv := newTestServer(toolExecutor{})
	defer srv.Close()

	resp, body := post(t, srv, testKey,
		`{"model":"test-model","max_tokens":64,"tool_choice":{"type":"any"},`+
			`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],`+
			`"messages":[{"role":"user","content":"weather in Paris?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	tool := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["name"] != "get_weather" || tool["id"] != "call_1" {
		t.Errorf("tool_use block = %v", tool)
	}
	if input := tool["input"].(map[string]any); input["city"] != "Paris" {
		t.Errorf("input = %v", tool["input"])
	}
}

// thinkExecutor emits a reasoning trace before its answer. Execute returns the
// buffered form (thinking block then text) so the gateway's buffered-stream
// fallback is exercised too.
type thinkExecutor struct{}

func (thinkExecutor) Execute(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{
		Blocks: []core.ContentBlock{
			core.ThinkingBlock("let me think", ""),
			core.TextBlock("the answer"),
		},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 3, OutputTokens: 5},
	}, nil
}

func (thinkExecutor) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	if err := sink.Thinking("let me "); err != nil {
		return err
	}
	if err := sink.Thinking("think"); err != nil {
		return err
	}
	if err := sink.Text("the answer"); err != nil {
		return err
	}
	return sink.Done(core.StopEndTurn, core.Usage{InputTokens: 3, OutputTokens: 5})
}

// blockTypesByIndex maps each content block's index to its type.
func blockTypesByIndex(events []sseEvent) map[float64]string {
	starts := map[float64]string{}
	for _, e := range events {
		if e.name == "content_block_start" {
			starts[e.data["index"].(float64)] = e.data["content_block"].(map[string]any)["type"].(string)
		}
	}
	return starts
}

func TestStreamThinking(t *testing.T) {
	srv := newTestServer(thinkExecutor{})
	defer srv.Close()

	resp, events := streamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},`+
			`"messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// thinking block at index 0, text at index 1.
	if blocks := blockTypesByIndex(events); blocks[0] != "thinking" || blocks[1] != "text" {
		t.Errorf("block types = %v", blocks)
	}

	// thinking_delta fragments concatenate to the full reasoning.
	var thinking string
	for _, e := range events {
		if e.name == "content_block_delta" {
			if d := e.data["delta"].(map[string]any); d["type"] == "thinking_delta" {
				thinking += d["thinking"].(string)
			}
		}
	}
	if thinking != "let me think" {
		t.Errorf("thinking = %q", thinking)
	}
}

func TestNonStreamingThinking(t *testing.T) {
	srv := newTestServer(thinkExecutor{})
	defer srv.Close()

	resp, body := post(t, srv, testKey,
		`{"model":"test-model","max_tokens":64,"thinking":{"type":"enabled"},`+
			`"messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	content := body["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "let me think" {
		t.Errorf("first block = %v", first)
	}
	if content[1].(map[string]any)["type"] != "text" {
		t.Errorf("second block = %v", content[1])
	}
}

func TestValidationErrors(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	tests := []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing model", `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"missing max_tokens", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"test-model","max_tokens":16,"messages":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := post(t, srv, testKey, tc.body)
			if resp.StatusCode != http.StatusBadRequest || errType(body) != "invalid_request_error" {
				t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
			}
		})
	}
}

func TestEngineErrorIsApiError(t *testing.T) {
	srv := newTestServer(&echoExecutor{err: io.ErrUnexpectedEOF})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusInternalServerError || errType(body) != "api_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func errType(body map[string]any) any {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return nil
	}
	return e["type"]
}
