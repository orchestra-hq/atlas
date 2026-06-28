package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// --no-cache also *refreshes* the cache, so a following normal run is a hit.
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL}, []string{"org/m"}); err != nil {
		t.Fatalf("post-refresh inspect: %v", err)
	}
	if hits["config.json"] != 2 {
		t.Errorf("config.json fetched %d times, want 2 (--no-cache should have refreshed the cache)", hits["config.json"])
	}
}

// ageCacheFiles back-dates every cache file's mtime so a TTL check sees them as
// stale, making the time-based expiry deterministic.
func ageCacheFiles(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "inspect-cache", "*.json"))
	if len(files) == 0 {
		t.Fatal("no cache files to age")
	}
	old := time.Now().Add(-age)
	for _, f := range files {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// A mutable ref ("main") whose cache entry is older than the TTL is refetched.
func TestInspectCacheMutableTTLExpires(t *testing.T) {
	hits := map[string]int{}
	srv := hfFixtureServer(t, defaultFixtures(), hits)
	dir := t.TempDir()

	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL}, []string{"org/m"}); err != nil {
		t.Fatal(err)
	}
	ageCacheFiles(t, dir, 48*time.Hour)
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL}, []string{"org/m"}); err != nil {
		t.Fatal(err)
	}
	if hits["config.json"] != 2 {
		t.Errorf("config.json fetched %d times, want 2 (stale mutable ref should refetch)", hits["config.json"])
	}
}

// An immutable commit SHA is cached indefinitely — age doesn't expire it.
func TestInspectCacheImmutableSHANeverExpires(t *testing.T) {
	hits := map[string]int{}
	srv := hfFixtureServer(t, defaultFixtures(), hits)
	dir := t.TempDir()
	sha := "0123456789abcdef0123456789abcdef01234567"

	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL, revision: sha}, []string{"org/m"}); err != nil {
		t.Fatal(err)
	}
	ageCacheFiles(t, dir, 48*time.Hour)
	if _, err := runInspectCapture(t, &inspectOptions{stateDir: dir, endpoint: srv.URL, revision: sha}, []string{"org/m"}); err != nil {
		t.Fatal(err)
	}
	if hits["config.json"] != 1 {
		t.Errorf("config.json fetched %d times, want 1 (immutable SHA cache must not expire)", hits["config.json"])
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

func TestInspectCommandGGUFRepo(t *testing.T) {
	// A minimal GGUF header: "GGUF" + version + tensor/kv counts + one
	// general.architecture string KV. Hand-encoded little-endian.
	header := buildMinimalGGUF(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[{"rfilename":"m-Q8_0.gguf"},{"rfilename":"m-Q4_K_M.gguf"}]}`))
	})
	mux.HandleFunc("/org/repo/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/org/repo/resolve/main/m-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m.gguf", time.Time{}, bytes.NewReader(header))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	out, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL}, []string{"org/repo"})
	if err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	for _, want := range []string{"Format:   gguf", "llama", "Quants:", "Q4_K_M"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// buildMinimalGGUF hand-encodes a tiny valid GGUF header with one
// general.architecture="llama" KV, enough to exercise the command end-to-end.
func buildMinimalGGUF(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	putU32(&b, 3) // version
	putU64(&b, 0) // tensor count
	putU64(&b, 1) // kv count
	putStr(&b, "general.architecture")
	putU32(&b, 8) // ggufString
	putStr(&b, "llama")
	return b.Bytes()
}

func putU32(b *bytes.Buffer, v uint32) {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	b.Write(t[:])
}

func putU64(b *bytes.Buffer, v uint64) {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], v)
	b.Write(t[:])
}

func putStr(b *bytes.Buffer, s string) {
	putU64(b, uint64(len(s)))
	b.WriteString(s)
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
