package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/modelmeta"
)

// hfFixtureServer serves a fixed set of metadata files and counts hits per file,
// so command tests can assert caching (one fetch vs two) and output.
func hfFixtureServer(t *testing.T, files map[string]string, hits map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		hits[name]++
		body, ok := files[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func defaultFixtures() map[string]string {
	return map[string]string{
		"config.json":            `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","max_position_embeddings":32768}`,
		"tokenizer_config.json":  `{"chat_template":"{{ messages }}"}`,
		"generation_config.json": `{"temperature":0.7,"top_p":0.8}`,
	}
}

func runInspectCapture(t *testing.T, opts *inspectOptions, args []string) (string, error) {
	t.Helper()
	cmd := testCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runInspect(context.Background(), cmd, opts, args)
	return out.String(), err
}

func TestInspectCommandPrintsPlan(t *testing.T) {
	hits := map[string]int{}
	srv := hfFixtureServer(t, defaultFixtures(), hits)
	opts := &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL}

	out, err := runInspectCapture(t, opts, []string{"org/qwen2.5-7b"})
	if err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	for _, want := range []string{"Qwen2ForCausalLM", "qwen2", "32768", "Template: present", "temperature=0.7", "Verdict:", "pending (M8 Phase 2)", "pending (M8 Phase 3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectCommandCaches(t *testing.T) {
	hits := map[string]int{}
	srv := hfFixtureServer(t, defaultFixtures(), hits)
	dir := t.TempDir()

	// First inspect fetches; second of the same repo@rev serves from the cache.
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL}, []string{"org/m"}); err != nil {
		t.Fatalf("first inspect: %v", err)
	}
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL}, []string{"org/m"}); err != nil {
		t.Fatalf("second inspect: %v", err)
	}
	if hits["config.json"] != 1 {
		t.Errorf("config.json fetched %d times, want 1 (second should hit cache)", hits["config.json"])
	}

	// --no-cache forces a refetch.
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL, noCache: true}, []string{"org/m"}); err != nil {
		t.Fatalf("no-cache inspect: %v", err)
	}
	if hits["config.json"] != 2 {
		t.Errorf("config.json fetched %d times, want 2 after --no-cache", hits["config.json"])
	}
}

func TestInspectCommandJSON(t *testing.T) {
	hits := map[string]int{}
	srv := hfFixtureServer(t, defaultFixtures(), hits)
	opts := &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL, asJSON: true}

	out, err := runInspectCapture(t, opts, []string{"org/m"})
	if err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	var res modelmeta.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON Result: %v\n%s", err, out)
	}
	if res.Capabilities.Architecture != "Qwen2ForCausalLM" {
		t.Errorf("json arch = %q", res.Capabilities.Architecture)
	}
}

func TestInspectCommandGatedError(t *testing.T) {
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_ = hits

	_, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL}, []string{"org/gated"})
	if err == nil || !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("expected gated/HF_TOKEN error, got %v", err)
	}
}
