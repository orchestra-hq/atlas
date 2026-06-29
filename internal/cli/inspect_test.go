package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	// The candidate engine (and thus the parser preview) is host-dependent for a
	// safetensors repo, so assert only the host-independent rows here; parser
	// rendering is covered by modelmeta.TestFamilyEngineArgs.
	for _, want := range []string{"Qwen2ForCausalLM", "qwen2", "32768", "Template: present", "temperature=0.7", "Verdict:", "family:   qwen2", "parsers:", "loadable: yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The load/fit dimensions are decided now, not deferred to a later phase.
	if strings.Contains(out, "pending (M8 Phase") {
		t.Errorf("no verdict row should still be pending:\n%s", out)
	}
}

// sizedRepoServer serves a qwen2 config.json and a blobs listing with one
// safetensors shard of the given size, so fit can be exercised deterministically.
func sizedRepoServer(t *testing.T, repo, arch, modelType string, shardBytes int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+repo+"/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"architectures":["` + arch + `"],"model_type":"` + modelType + `","max_position_embeddings":32768}`))
	})
	mux.HandleFunc("/api/models/"+repo, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"siblings":[{"rfilename":"model.safetensors","size":%d}]}`, shardBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestInspectFitsVerdict(t *testing.T) {
	const fourGiB = 4 << 30
	srv := sizedRepoServer(t, "org/m", "Qwen2ForCausalLM", "qwen2", fourGiB)

	// A roomy target fits; a tiny one does not. --vram makes the check deterministic
	// regardless of the host running the test.
	fits, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL, vram: 80}, []string{"org/m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fits, "fits:     yes") {
		t.Errorf("want fits yes against 80 GiB:\n%s", fits)
	}
	noFit, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL, vram: 1}, []string{"org/m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noFit, "fits:     no") {
		t.Errorf("want fits no against 1 GiB:\n%s", noFit)
	}
}

// TestInspectUnknownFamilyPointer proves inspect previews the P8.4 contribution
// funnel: a loadable, fitting, unknown-family model shows the "add a family" pointer
// (Mistral is loadable on both the macOS mlx and Linux vllm candidate engines but is
// not a known family, so this is host-independent), while a known family shows none.
func TestInspectUnknownFamilyPointer(t *testing.T) {
	unknown := sizedRepoServer(t, "org/mistral", "MistralForCausalLM", "mistral", 4<<30)
	out, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: unknown.URL, vram: 80}, []string{"org/mistral"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "family:   unknown") {
		t.Fatalf("expected an unknown family for Mistral:\n%s", out)
	}
	if !strings.Contains(out, "internal/modelmeta/family.go") {
		t.Errorf("a loadable+fitting unknown family should show the contribution pointer:\n%s", out)
	}

	// A known family (qwen2) carries no family pointer — its parsers are configured.
	known := sizedRepoServer(t, "org/q2", "Qwen2ForCausalLM", "qwen2", 4<<30)
	out2, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: known.URL, vram: 80}, []string{"org/q2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "internal/modelmeta/family.go") {
		t.Errorf("a known family should not show the add-a-family pointer:\n%s", out2)
	}
}

// Regression for the P8.3 review: a cache entry written by an older binary carries
// a stale Verdict (here Loadable="pending"); inspect must recompute the verdict
// from the cached Capabilities rather than display the stale value.
func TestInspectCacheHitRecomputesLoadable(t *testing.T) {
	dir := t.TempDir()
	repo := "org/cached"
	stale := modelmeta.Result{
		Capabilities: modelmeta.Capabilities{
			Repo: repo, Revision: "main", Format: modelmeta.FormatSafetensors,
			Architecture: "Qwen2ForCausalLM", ModelType: "qwen2", Engines: []string{"vllm"},
		},
		Verdict: modelmeta.Verdict{Conclusion: modelmeta.ConclusionInspected, Engine: "vllm", Family: "qwen2", Loadable: modelmeta.Pending, Fits: modelmeta.Pending},
	}
	writeInspectCache(dir, repo, "main", stale)

	// No endpoint: a cache miss would fail to fetch, so a clean run proves the hit.
	out, err := runInspectCapture(t, &inspectOptions{stateDir: dir, vram: 80}, []string{repo})
	if err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	if !strings.Contains(out, "loadable: yes") {
		t.Errorf("stale cached Loadable should be recomputed to yes:\n%s", out)
	}
	if strings.Contains(out, "loadable: pending") {
		t.Errorf("stale 'pending' Loadable leaked from the cache:\n%s", out)
	}
}

func TestInspectUnsupportedArch(t *testing.T) {
	srv := sizedRepoServer(t, "org/exotic", "FooBarForCausalLM", "foobar", 1<<30)
	out, err := runInspectCapture(t, &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL, vram: 80}, []string{"org/exotic"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "loadable: no") {
		t.Errorf("an unsupported architecture should be loadable: no:\n%s", out)
	}
	if !strings.Contains(out, "open a PR") {
		t.Errorf("the not-loadable reason should point at the contribution path:\n%s", out)
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
	const twoGiB = 2 << 30
	srv := sizedRepoServer(t, "org/m", "Qwen2ForCausalLM", "qwen2", twoGiB)
	opts := &inspectOptions{stateDir: t.TempDir(), endpoint: srv.URL, asJSON: true, vram: 80}

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
	if res.Capabilities.WeightBytes != twoGiB {
		t.Errorf("json weight_bytes = %d, want %d", res.Capabilities.WeightBytes, twoGiB)
	}
	if res.Verdict.Fits != "yes" {
		t.Errorf("json fits = %q, want yes (live, against the --vram override)", res.Verdict.Fits)
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
