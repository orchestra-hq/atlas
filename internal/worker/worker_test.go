package worker

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
)

// TestMain lets the test binary impersonate llama-server: when ATLAS_FAKE_LLAMA
// is set, it serves a minimal /health + /v1/chat/completions instead of running
// the suite. Worker.Start then supervises os.Args[0] as if it were the engine.
func TestMain(m *testing.M) {
	if behavior := os.Getenv("ATLAS_FAKE_LLAMA"); behavior != "" {
		runFakeLlama(behavior)
		return
	}
	os.Exit(m.Run())
}

func runFakeLlama(behavior string) {
	if behavior == "crash" {
		os.Exit(3)
	}

	// llama-server's flags; we only need --port, ignore the rest.
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.String("port", "0", "")
	// Allow unknown flags (--jinja, -m, --host, ...) by pre-filtering.
	_ = fs.Parse(filterPortArg(os.Args[1:]))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if behavior == "loading" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("content-type", "text/event-stream")
			for _, tok := range []string{"po", "ng"} {
				_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+tok+`"},"finish_reason":null}]}`+"\n\n")
			}
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	_ = http.ListenAndServe("127.0.0.1:"+*port, mux)
	os.Exit(0)
}

// filterPortArg reduces the engine arg list to just --port <n> so the fake's
// minimal flag set parses cleanly.
func filterPortArg(args []string) []string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			return []string{"--port", args[i+1]}
		}
	}
	return nil
}

func fakeConfig(t *testing.T, behavior string) Config {
	t.Helper()
	t.Setenv("ATLAS_FAKE_LLAMA", behavior)
	return Config{
		BinPath:      os.Args[0],
		Model:        "fake-model",
		LogPath:      filepath.Join(t.TempDir(), "engine.log"),
		ReadyTimeout: 5 * time.Second,
	}
}

func TestEngineSetupArgs(t *testing.T) {
	base := Config{Host: "127.0.0.1", Port: 8000, Model: "m", ExtraArgs: []string{"--flag"}}

	t.Run("llamacpp", func(t *testing.T) {
		cfg := base
		cfg.Engine = EngineLlamaCpp
		cfg.ModelArgs = []string{"-hf", "repo:Q4"}
		args, adapter, err := engineSetup(cfg)
		if err != nil {
			t.Fatalf("engineSetup: %v", err)
		}
		if adapter == nil {
			t.Fatal("nil adapter")
		}
		// --host/--port precede the model args; --jinja present; extra appended.
		want := []string{"--host", "127.0.0.1", "--port", "8000", "--jinja", "-hf", "repo:Q4", "--flag"}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})

	t.Run("vllm", func(t *testing.T) {
		cfg := base
		cfg.Engine = EngineVLLM
		cfg.ModelArgs = []string{"Qwen/Qwen2.5-1.5B-Instruct"}
		args, adapter, err := engineSetup(cfg)
		if err != nil {
			t.Fatalf("engineSetup: %v", err)
		}
		if adapter == nil {
			t.Fatal("nil adapter")
		}
		// `serve <model> --host H --port P [extra]`: model is positional.
		want := []string{"serve", "Qwen/Qwen2.5-1.5B-Instruct", "--host", "127.0.0.1", "--port", "8000", "--flag"}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		cfg := base
		cfg.Engine = Engine("sglang")
		if _, _, err := engineSetup(cfg); err == nil {
			t.Fatal("expected error for unknown engine")
		}
	})
}

func TestStartVLLMEngineExecutes(t *testing.T) {
	cfg := fakeConfig(t, "ready")
	cfg.Engine = EngineVLLM
	cfg.ModelArgs = []string{"fake-model"} // positional model ref for `vllm serve`
	w, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })

	resp, err := w.Execute(context.Background(), core.Request{
		Model:     "fake-model",
		MaxTokens: 16,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("ping")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Text() != "pong" || resp.StopReason != core.StopEndTurn {
		t.Errorf("resp = %q / %q", resp.Text(), resp.StopReason)
	}
}

func TestStartReadyAndExecute(t *testing.T) {
	w, err := Start(context.Background(), fakeConfig(t, "ready"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })

	resp, err := w.Execute(context.Background(), core.Request{
		Model:     "fake-model",
		MaxTokens: 16,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("ping")}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Text() != "pong" || resp.StopReason != core.StopEndTurn {
		t.Errorf("resp = %q / %q", resp.Text(), resp.StopReason)
	}
}

// collectSink records streamed deltas and the terminal signal.
type collectSink struct {
	text   string
	reason core.StopReason
	usage  core.Usage
}

func (c *collectSink) Thinking(_ string) error                { return nil }
func (c *collectSink) Text(d string) error                    { c.text += d; return nil }
func (c *collectSink) ToolCallStart(_ int, _, _ string) error { return nil }
func (c *collectSink) ToolCallDelta(_ int, _ string) error    { return nil }
func (c *collectSink) Done(r core.StopReason, u core.Usage) error {
	c.reason, c.usage = r, u
	return nil
}

func TestStartReadyAndStream(t *testing.T) {
	w, err := Start(context.Background(), fakeConfig(t, "ready"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })

	sink := &collectSink{}
	if err := w.ExecuteStream(context.Background(), core.Request{
		Model:     "fake-model",
		MaxTokens: 16,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("ping")}}},
	}, sink); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if sink.text != "pong" || sink.reason != core.StopEndTurn {
		t.Errorf("streamed = %q / %q", sink.text, sink.reason)
	}
	if sink.usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", sink.usage)
	}
}

func TestStartCrashSurfaces(t *testing.T) {
	_, err := Start(context.Background(), fakeConfig(t, "crash"))
	if err == nil {
		t.Fatal("expected error when engine exits before ready")
	}
}

func TestStartReadyTimeout(t *testing.T) {
	cfg := fakeConfig(t, "loading")
	cfg.ReadyTimeout = 1 * time.Second
	start := time.Now()
	_, err := Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected timeout error when engine never reports healthy")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timed out after %s, expected ~1s", elapsed)
	}
}

func TestStopIdempotent(t *testing.T) {
	w, err := Start(context.Background(), fakeConfig(t, "ready"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestEndpoint(t *testing.T) {
	w := &Worker{cfg: Config{Host: "127.0.0.1", Port: 9999}}
	if got := w.Endpoint(); got != "http://127.0.0.1:9999" {
		t.Errorf("Endpoint() = %q", got)
	}
}
