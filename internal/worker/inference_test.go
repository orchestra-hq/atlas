package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/server"
)

// fakeEngine is a worker.Inferencer with per-call behaviour set by the test, so
// one registered model can stand in for any engine response.
type fakeEngine struct {
	execute func(ctx context.Context, req core.Request) (core.Response, error)
	stream  func(ctx context.Context, req core.Request, sink core.StreamSink) error
	count   func(ctx context.Context, req core.Request) (int, error)
}

func (f *fakeEngine) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	return f.execute(ctx, req)
}

func (f *fakeEngine) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	return f.stream(ctx, req, sink)
}

func (f *fakeEngine) CountTokens(ctx context.Context, req core.Request) (int, error) {
	return f.count(ctx, req)
}

// recordingSink captures streamed deltas for assertions.
type recordingSink struct {
	text   strings.Builder
	tools  []string
	reason core.StopReason
	usage  core.Usage
}

func (r *recordingSink) Thinking(string) error { return nil }
func (r *recordingSink) Text(d string) error   { r.text.WriteString(d); return nil }
func (r *recordingSink) ToolCallStart(_ int, id, name string) error {
	r.tools = append(r.tools, id+":"+name)
	return nil
}
func (r *recordingSink) ToolCallDelta(int, string) error { return nil }
func (r *recordingSink) Done(reason core.StopReason, usage core.Usage) error {
	r.reason, r.usage = reason, usage
	return nil
}

// dialedModel stands up a real hub and a real worker.Dial serving eng, and
// returns the gateway-side Model (its Exec is the remoteWorker over the live WS
// connection) plus a teardown.
func dialedModel(t *testing.T, eng Inferencer) (server.Model, func()) {
	t.Helper()
	reg := &capturingRegistry{models: map[string]server.Model{}}
	hub := server.NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan struct{})
	go func() {
		_ = Dial(ctx, DialConfig{
			ServerURL: url,
			Token:     "tok",
			Name:      "infworker",
			Models:    []ServedModel{{Name: "m", ContextWindow: 4096, Engine: eng}},
		})
		close(dialDone)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := reg.get("m"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("model never registered with the gateway")
		}
		time.Sleep(5 * time.Millisecond)
	}
	m, _ := reg.get("m")

	return m, func() {
		cancel()
		<-dialDone
		ts.Close()
	}
}

type capturingRegistry struct {
	mu     sync.Mutex
	models map[string]server.Model
}

func (r *capturingRegistry) RegisterModel(m server.Model) {
	r.mu.Lock()
	r.models[m.Name] = m
	r.mu.Unlock()
}

func (r *capturingRegistry) UnregisterModel(name string) {
	r.mu.Lock()
	delete(r.models, name)
	r.mu.Unlock()
}

func (r *capturingRegistry) get(name string) (server.Model, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.models[name]
	return m, ok
}

func textReq() core.Request {
	return core.Request{Model: "m", MaxTokens: 32, Messages: []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}}}
}

// TestRemoteExecute_buffered drives a buffered Execute over the live channel.
func TestRemoteExecute_buffered(t *testing.T) {
	eng := &fakeEngine{execute: func(_ context.Context, req core.Request) (core.Response, error) {
		return core.Response{
			Blocks:     []core.ContentBlock{core.TextBlock("answer for " + req.System)},
			StopReason: core.StopEndTurn,
			Usage:      core.Usage{InputTokens: 4, OutputTokens: 6},
		}, nil
	}}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	req := textReq()
	req.System = "tag"
	resp, err := m.Exec.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Text() != "answer for tag" || resp.StopReason != core.StopEndTurn || resp.Usage.OutputTokens != 6 {
		t.Errorf("response = %+v", resp)
	}
}

// TestRemoteExecute_stream drives a streaming ExecuteStream, asserting deltas
// and the closing usage/reason arrive over the wire in order.
func TestRemoteExecute_stream(t *testing.T) {
	eng := &fakeEngine{stream: func(_ context.Context, _ core.Request, sink core.StreamSink) error {
		_ = sink.Text("hello ")
		_ = sink.Text("world")
		_ = sink.ToolCallStart(0, "tu_1", "get_weather")
		_ = sink.ToolCallDelta(0, `{"city":"NYC"}`)
		return sink.Done(core.StopToolUse, core.Usage{InputTokens: 3, OutputTokens: 9})
	}}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	streamer, ok := m.Exec.(server.StreamExecutor)
	if !ok {
		t.Fatal("remote worker is not a StreamExecutor")
	}
	rec := &recordingSink{}
	if err := streamer.ExecuteStream(context.Background(), textReq(), rec); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if rec.text.String() != "hello world" {
		t.Errorf("streamed text = %q, want %q", rec.text.String(), "hello world")
	}
	if len(rec.tools) != 1 || rec.tools[0] != "tu_1:get_weather" {
		t.Errorf("streamed tools = %v", rec.tools)
	}
	if rec.reason != core.StopToolUse || rec.usage.OutputTokens != 9 {
		t.Errorf("final reason/usage = %v / %+v", rec.reason, rec.usage)
	}
}

// TestRemoteCountTokens drives a count_tokens round-trip.
func TestRemoteCountTokens(t *testing.T) {
	eng := &fakeEngine{count: func(context.Context, core.Request) (int, error) { return 42, nil }}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	tc, ok := m.Exec.(server.TokenCounter)
	if !ok {
		t.Fatal("remote worker is not a TokenCounter")
	}
	n, err := tc.CountTokens(context.Background(), textReq())
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}
}

// TestRemoteExecute_engineUnavailable confirms a worker-reported unavailable
// engine maps back to the gateway's retryable error.
func TestRemoteExecute_engineUnavailable(t *testing.T) {
	eng := &fakeEngine{execute: func(context.Context, core.Request) (core.Response, error) {
		return core.Response{}, core.ErrEngineUnavailable
	}}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	_, err := m.Exec.Execute(context.Background(), textReq())
	if !errors.Is(err, core.ErrEngineUnavailable) {
		t.Errorf("Execute error = %v, want ErrEngineUnavailable", err)
	}
}

// TestRemoteExecute_cancelPropagates confirms cancelling the gateway-side
// request cancels the engine call on the worker.
func TestRemoteExecute_cancelPropagates(t *testing.T) {
	engineSawCancel := make(chan struct{})
	eng := &fakeEngine{stream: func(ctx context.Context, _ core.Request, sink core.StreamSink) error {
		_ = sink.Text("starting")
		<-ctx.Done() // block until the gateway cancels
		close(engineSawCancel)
		return ctx.Err()
	}}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	streamer := m.Exec.(server.StreamExecutor)
	ctx, cancel := context.WithCancel(context.Background())
	streamDone := make(chan error, 1)
	go func() { streamDone <- streamer.ExecuteStream(ctx, textReq(), &recordingSink{}) }()

	// Let the first delta arrive, then cancel; the engine's ctx should fire.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-engineSawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not observe cancellation")
	}
	select {
	case err := <-streamDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ExecuteStream returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteStream did not return after cancel")
	}
}

// TestRemoteExecute_concurrent fires many requests over the one connection at
// once, asserting each response is demuxed back to its own caller by request id.
func TestRemoteExecute_concurrent(t *testing.T) {
	eng := &fakeEngine{execute: func(_ context.Context, req core.Request) (core.Response, error) {
		return core.Response{
			Blocks:     []core.ContentBlock{core.TextBlock(req.System)},
			StopReason: core.StopEndTurn,
		}, nil
	}}
	m, teardown := dialedModel(t, eng)
	defer teardown()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			req := textReq()
			req.System = tag
			resp, err := m.Exec.Execute(context.Background(), req)
			if err != nil {
				errs <- err
				return
			}
			if resp.Text() != tag {
				errs <- errors.New("got " + resp.Text() + " want " + tag)
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
