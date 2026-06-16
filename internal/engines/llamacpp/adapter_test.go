package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// fakeServer stands in for llama-server's /v1/chat/completions endpoint.
func fakeServer(t *testing.T, status int, respBody string, capture *chatRequest) *httptest.Server {
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

func temp(f float64) *float64 { return &f }

func TestExecuteTranslatesAndMaps(t *testing.T) {
	var got chatRequest
	srv := fakeServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":2}
	}`, &got)
	defer srv.Close()

	a := New(srv.URL, "served-model", srv.Client())
	resp, err := a.Execute(context.Background(), core.Request{
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

	// Response mapping.
	if resp.Text() != "hello world" {
		t.Errorf("text = %q", resp.Text())
	}
	if resp.StopReason != core.StopEndTurn {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Request translation: system message prepended, sampling forwarded.
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
		"stop":   core.StopEndTurn,
		"length": core.StopMaxTokens,
		"":       core.StopEndTurn,
		"weird":  core.StopEndTurn,
	}
	for reason, want := range cases {
		if got := mapFinishReason(reason); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestExecuteEngineErrorStatus(t *testing.T) {
	srv := fakeServer(t, http.StatusServiceUnavailable, `{"error":"loading model"}`, nil)
	defer srv.Close()

	a := New(srv.URL, "m", srv.Client())
	_, err := a.Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestExecuteNoChoices(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, `{"choices":[],"usage":{}}`, nil)
	defer srv.Close()

	a := New(srv.URL, "m", srv.Client())
	_, err := a.Execute(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	})
	if err == nil {
		t.Fatal("expected error when no choices returned")
	}
}

// recordSink captures the deltas and terminal signal a stream produces.
type recordSink struct {
	deltas    []string
	reason    core.StopReason
	usage     core.Usage
	done      bool
	stopAfter int // return ErrStopStreaming after this many deltas (0 = never)
}

func (s *recordSink) Text(delta string) error {
	s.deltas = append(s.deltas, delta)
	if s.stopAfter > 0 && len(s.deltas) >= s.stopAfter {
		return core.ErrStopStreaming
	}
	return nil
}

func (s *recordSink) Done(reason core.StopReason, usage core.Usage) error {
	s.done = true
	s.reason = reason
	s.usage = usage
	return nil
}

// sseServer streams the given raw event-stream body.
func sseServer(t *testing.T, body string, capture *chatRequest) *httptest.Server {
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

func TestExecuteStreamForwardsDeltasAndUsage(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var got chatRequest
	srv := sseServer(t, body, &got)
	defer srv.Close()

	sink := &recordSink{}
	a := New(srv.URL, "m", srv.Client())
	err := a.ExecuteStream(context.Background(), core.Request{
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
	a := New(srv.URL, "m", srv.Client())
	if err := a.ExecuteStream(context.Background(), core.Request{
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

func TestExecuteStreamErrorStatus(t *testing.T) {
	srv := fakeServer(t, http.StatusServiceUnavailable, `{"error":"loading"}`, nil)
	defer srv.Close()

	a := New(srv.URL, "m", srv.Client())
	err := a.ExecuteStream(context.Background(), core.Request{
		Model:     "m",
		MaxTokens: 8,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}},
	}, &recordSink{})
	if err == nil {
		t.Fatal("expected error on non-200 stream start")
	}
}
