package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// chatPost issues a non-streaming POST /v1/chat/completions.
func chatPost(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
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

// chatStreamPost issues a streaming POST /v1/chat/completions and returns the
// parsed chunk objects (the [DONE] sentinel is dropped).
func chatStreamPost(t *testing.T, srv *httptest.Server, body string) (*http.Response, []map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
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
	var chunks []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		line := strings.TrimSpace(block)
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		chunks = append(chunks, obj)
	}
	return resp, chunks
}

// firstChoice returns choices[0] of a chunk, or nil if choices is empty.
func firstChoice(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	return choices[0].(map[string]any)
}

func TestChatCompletionHappyPath(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "hello there"})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey,
		`{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["object"] != "chat.completion" || body["model"] != testModel {
		t.Errorf("envelope = %v", body)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "hello there" {
		t.Errorf("message = %v", msg)
	}
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 7 || usage["completion_tokens"].(float64) != 5 {
		t.Errorf("usage = %v", usage)
	}
}

func TestChatCompletionAuthAndModelErrors(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "x"})
	defer srv.Close()

	resp, body := chatPost(t, srv, "wrong",
		`{"model":"test-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusUnauthorized || errType(body) != "authentication_error" {
		t.Errorf("auth: status = %d, type = %v", resp.StatusCode, errType(body))
	}

	resp, body = chatPost(t, srv, testKey,
		`{"model":"nope","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound || errType(body) != "not_found_error" {
		t.Errorf("model: status = %d, type = %v", resp.StatusCode, errType(body))
	}

	resp, body = chatPost(t, srv, testKey, `{not json`)
	if resp.StatusCode != http.StatusBadRequest || errType(body) != "invalid_request_error" {
		t.Errorf("badjson: status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

func TestChatCompletionStopSequence(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "one two three four"})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey,
		`{"model":"test-model","max_tokens":16,"stop":"three","messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	// A gateway stop-sequence match maps to finish_reason "stop".
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	if got := choice["message"].(map[string]any)["content"]; got != "one two " {
		t.Errorf("content = %q", got)
	}
}

func TestChatCompletionMaxTokensFinishReason(t *testing.T) {
	srv := newTestServer(&lengthExecutor{})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey,
		`{"model":"test-model","max_tokens":4,"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", choice["finish_reason"])
	}
}

// lengthExecutor returns a max_tokens stop reason.
type lengthExecutor struct{}

func (lengthExecutor) Execute(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock("truncated")},
		StopReason: core.StopMaxTokens,
		Usage:      core.Usage{InputTokens: 3, OutputTokens: 4},
	}, nil
}

func TestChatCompletionNonStreamingToolUse(t *testing.T) {
	srv := newTestServer(toolExecutor{})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey, `{
		"model":"test-model","max_tokens":64,"tool_choice":"required",
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"weather in Paris?"}]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	calls := choice["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d", len(calls))
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_1" || fn["name"] != "get_weather" {
		t.Errorf("tool_call = %v", call)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["city"] != "Paris" {
		t.Errorf("args = %v", args)
	}
}

func TestChatCompletionStreamText(t *testing.T) {
	srv := newTestServer(&streamExecutor{deltas: []string{"stream ", "me ", "please"}})
	defer srv.Close()

	resp, chunks := chatStreamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	if first := firstChoice(chunks[0]); first == nil || first["delta"].(map[string]any)["role"] != "assistant" {
		t.Errorf("first chunk = %v", chunks[0])
	}

	var text string
	var finish any
	var usageSeen bool
	for _, ch := range chunks {
		if ch["object"] != "chat.completion.chunk" {
			t.Errorf("object = %v", ch["object"])
		}
		c := firstChoice(ch)
		if c != nil {
			if d, ok := c["delta"].(map[string]any)["content"].(string); ok {
				text += d
			}
			if c["finish_reason"] != nil {
				finish = c["finish_reason"]
			}
		} else if ch["usage"] != nil {
			usageSeen = true
		}
	}
	if text != "stream me please" {
		t.Errorf("text = %q", text)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %v", finish)
	}
	if !usageSeen {
		t.Errorf("expected a usage chunk with include_usage")
	}
}

func TestChatCompletionStreamToolUse(t *testing.T) {
	srv := newTestServer(toolExecutor{})
	defer srv.Close()

	resp, chunks := chatStreamPost(t, srv, `{
		"model":"test-model","max_tokens":64,"stream":true,"tool_choice":"required",
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"weather in Paris?"}]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Accumulate tool-call name + arguments by index, OpenAI-SDK style.
	names := map[float64]string{}
	args := map[float64]string{}
	var finish any
	for _, ch := range chunks {
		c := firstChoice(ch)
		if c == nil {
			continue
		}
		if c["finish_reason"] != nil {
			finish = c["finish_reason"]
		}
		tcs, ok := c["delta"].(map[string]any)["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, raw := range tcs {
			tc := raw.(map[string]any)
			idx := tc["index"].(float64)
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if n, ok := fn["name"].(string); ok && n != "" {
				names[idx] = n
			}
			if a, ok := fn["arguments"].(string); ok {
				args[idx] += a
			}
		}
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %v", finish)
	}
	if names[0] != "get_weather" {
		t.Errorf("tool name = %v", names)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args[0]), &parsed); err != nil {
		t.Fatalf("args %q invalid: %v", args[0], err)
	}
	if parsed["city"] != "Paris" {
		t.Errorf("args = %v", parsed)
	}
}

func TestChatCompletionStreamBufferedFallback(t *testing.T) {
	// echoExecutor implements only Executor; streaming must still work.
	srv := newTestServer(&echoExecutor{reply: "buffered reply"})
	defer srv.Close()

	resp, chunks := chatStreamPost(t, srv,
		`{"model":"test-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var text string
	for _, ch := range chunks {
		if c := firstChoice(ch); c != nil {
			if d, ok := c["delta"].(map[string]any)["content"].(string); ok {
				text += d
			}
		}
	}
	if text != "buffered reply" {
		t.Errorf("text = %q", text)
	}
}

func TestChatCompletionEngineUnavailable529(t *testing.T) {
	srv := newTestServer(&echoExecutor{err: fmt.Errorf("boom: %w", core.ErrEngineUnavailable)})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey,
		`{"model":"test-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != statusOverloaded || errType(body) != "overloaded_error" {
		t.Errorf("status = %d, type = %v, want 529 overloaded_error", resp.StatusCode, errType(body))
	}
}

func TestChatCompletionContextWindowRejected(t *testing.T) {
	srv := registryServer(
		[]Model{{Name: testModel, Exec: &countingExecutor{reply: "x", count: 100}, ContextWindow: 200}},
		nil,
	)
	defer srv.Close()

	// input (100) + max_tokens (150) > window (200): rejected, OpenAI-shaped 400.
	resp, body := chatPost(t, srv, testKey,
		`{"model":"test-model","max_tokens":150,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest || errType(body) != "invalid_request_error" {
		t.Errorf("status = %d, type = %v", resp.StatusCode, errType(body))
	}
}

// TestChatCompletionDispatchesCanonicalForAlias verifies the OpenAI surface
// dispatches to the worker under the canonical served name, not the client's
// alias — a remote worker routes by req.Model and would reject the alias.
func TestChatCompletionDispatchesCanonicalForAlias(t *testing.T) {
	exec := &echoExecutor{reply: "hi"}
	srv := registryServer([]Model{{Name: testModel, Exec: exec}}, map[string]string{"gpt-4o": testModel})
	defer srv.Close()

	resp, body := chatPost(t, srv, testKey,
		`{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	// The executor (a remote worker) must see the canonical name.
	if exec.gotReq.Model != testModel {
		t.Errorf("executor saw model %q, want canonical %q", exec.gotReq.Model, testModel)
	}
	// The response still echoes the alias the client addressed.
	if body["model"] != "gpt-4o" {
		t.Errorf("response model = %v, want the requested alias gpt-4o", body["model"])
	}
}
