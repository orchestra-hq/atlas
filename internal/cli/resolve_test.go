package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/store"
	"github.com/orchestra-hq/atlas/internal/worker"
)

func testCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	return c
}

func TestResolveModelRawSpecFallsBack(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(t.TempDir())

	// Inspection is best-effort: point it at a host that 404s everything so the
	// metadata fetch fails and resolution falls back to the pre-M8 bare passthrough.
	t.Setenv("ATLAS_HF_ENDPOINT", notFoundServer(t).URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if rm.served != "ggml-org/Qwen2.5-0.5B-Instruct-GGUF" {
		t.Errorf("served = %q", rm.served)
	}
	if !reflect.DeepEqual(rm.modelArgs, []string{"-hf", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"}) {
		t.Errorf("modelArgs = %v", rm.modelArgs)
	}
	if rm.ctxHint != 0 || len(rm.engineArgs) != 0 || rm.reasoning {
		t.Errorf("expected bare plan, got ctxHint=%d engineArgs=%v reasoning=%v", rm.ctxHint, rm.engineArgs, rm.reasoning)
	}
}

// notFoundServer is an httptest server that 404s every request, used to force the
// best-effort metadata inspection to fail.
func notFoundServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveRawAutoConfigsKnownFamily proves a known-family HF repo that is NOT
// in the catalog is auto-configured with the family's parser engine_args,
// reasoning gating, sampling defaults, and context hint — the heart of P8.2.
func TestResolveRawAutoConfigsKnownFamily(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	hits := map[string]int{}
	srv := hfFixtureServer(t, map[string]string{
		"config.json":            `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","max_position_embeddings":40960}`,
		"tokenizer_config.json":  `{"chat_template":"{{ messages }}"}`,
		"generation_config.json": `{"temperature":0.6,"top_p":0.95}`,
	}, hits)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "Qwen/Qwen3-8B")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if rm.served != "Qwen/Qwen3-8B" || !reflect.DeepEqual(rm.modelArgs, []string{"Qwen/Qwen3-8B"}) {
		t.Errorf("served/modelArgs = %q / %v", rm.served, rm.modelArgs)
	}
	if !containsPair(rm.engineArgs, "--tool-call-parser", "hermes") {
		t.Errorf("engineArgs missing vLLM tool-call parser: %v", rm.engineArgs)
	}
	if !containsPair(rm.engineArgs, "--reasoning-parser", "qwen3") {
		t.Errorf("engineArgs missing reasoning parser: %v", rm.engineArgs)
	}
	if !rm.reasoning {
		t.Error("reasoning = false, want true for qwen3")
	}
	if rm.ctxHint != 40960 {
		t.Errorf("ctxHint = %d, want 40960 (from max_position_embeddings)", rm.ctxHint)
	}
	if rm.sampling.Temperature == nil || *rm.sampling.Temperature != 0.6 {
		t.Errorf("sampling temperature = %v, want 0.6", rm.sampling.Temperature)
	}
}

// A different engine renders the same family's parser names for that engine.
func TestResolveRawAutoConfigsPerEngine(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	hits := map[string]int{}
	srv := hfFixtureServer(t, map[string]string{
		"config.json": `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","max_position_embeddings":40960}`,
	}, hits)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineSGLang, st, cat, t.TempDir(), "", "Qwen/Qwen3-8B")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !containsPair(rm.engineArgs, "--tool-call-parser", "qwen25") {
		t.Errorf("SGLang engineArgs = %v, want qwen25 tool-call parser", rm.engineArgs)
	}
	if contains(rm.engineArgs, "--enable-auto-tool-choice") {
		t.Errorf("SGLang must not carry vLLM's --enable-auto-tool-choice: %v", rm.engineArgs)
	}
}

// A local .gguf whose header names a known family is auto-configured too — here
// proving reasoning gating is applied to a bring-your-own hybrid GGUF (the gap
// catalog/starter.yaml's qwen3-8b-gguf comment describes), with no parser flags
// since llama.cpp drives tool-calling from the embedded template.
func TestResolveRawAutoConfigsLocalGGUF(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())

	path := filepath.Join(t.TempDir(), "byo-qwen3.gguf")
	if err := os.WriteFile(path, qwen3GGUFHeader(t), 0o644); err != nil {
		t.Fatal(err)
	}

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", path)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !rm.reasoning {
		t.Error("reasoning = false, want true for a qwen3 GGUF")
	}
	if len(rm.engineArgs) != 0 {
		t.Errorf("engineArgs = %v, want none (template-driven)", rm.engineArgs)
	}
	if !reflect.DeepEqual(rm.modelArgs, []string{"-m", path}) {
		t.Errorf("modelArgs = %v", rm.modelArgs)
	}
}

// Negative control: an unknown family falls back to the bare plan unchanged,
// identical to pre-M8 behaviour, even though metadata was fetched successfully.
func TestResolveRawUnknownFamilyFallsBack(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	hits := map[string]int{}
	srv := hfFixtureServer(t, map[string]string{
		"config.json": `{"architectures":["MambaForCausalLM"],"model_type":"mamba","max_position_embeddings":8192}`,
	}, hits)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "org/mamba")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if len(rm.engineArgs) != 0 || rm.reasoning || rm.ctxHint != 0 {
		t.Errorf("expected bare plan for unknown family, got engineArgs=%v reasoning=%v ctxHint=%d", rm.engineArgs, rm.reasoning, rm.ctxHint)
	}
}

// multiQuantGGUFServer serves an HF GGUF repo with several quantization files:
// config.json 404s (not a transformers repo), the model API lists the quants, and
// each .gguf resolve URL serves the same minimal header so inspection succeeds.
func multiQuantGGUFServer(t *testing.T, repo string, files []string, header []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/"+repo, func(w http.ResponseWriter, _ *http.Request) {
		parts := make([]string, len(files))
		for i, f := range files {
			parts[i] = `{"rfilename":"` + f + `"}`
		}
		_, _ = w.Write([]byte(`{"siblings":[` + strings.Join(parts, ",") + `]}`))
	})
	mux.HandleFunc("/"+repo+"/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	for _, f := range files {
		mux.HandleFunc("/"+repo+"/resolve/main/"+f, func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, f, time.Time{}, bytes.NewReader(header))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A multi-quant GGUF repo with no --quant serves the Q4_K_M-preferring default,
// rewriting the llama.cpp args to -hf repo:<quant> (matching inspect's report).
func TestResolveRawQuantDefault(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	repo := "org/qwen3-gguf"
	srv := multiQuantGGUFServer(t, repo, []string{"m-Q4_K_M.gguf", "m-Q5_K_M.gguf", "m-Q8_0.gguf"}, qwen3GGUFHeader(t))
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", repo)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !reflect.DeepEqual(rm.modelArgs, []string{"-hf", repo + ":Q4_K_M"}) {
		t.Errorf("modelArgs = %v, want -hf %s:Q4_K_M", rm.modelArgs, repo)
	}
	if !rm.reasoning {
		t.Error("reasoning = false, want true for qwen3 GGUF")
	}
}

// An explicit --quant selects that quantization, normalized to the repo's
// canonical token even when the user types it lower-case.
func TestResolveRawQuantExplicit(t *testing.T) {
	cat, _ := catalog.Load()
	repo := "org/qwen3-gguf"
	for _, quant := range []string{"Q5_K_M", "q5_k_m"} {
		t.Run(quant, func(t *testing.T) {
			st := store.New(t.TempDir())
			srv := multiQuantGGUFServer(t, repo, []string{"m-Q4_K_M.gguf", "m-Q5_K_M.gguf", "m-Q8_0.gguf"}, qwen3GGUFHeader(t))
			t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

			rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), quant, repo)
			if err != nil {
				t.Fatalf("resolveModel: %v", err)
			}
			if !reflect.DeepEqual(rm.modelArgs, []string{"-hf", repo + ":Q5_K_M"}) {
				t.Errorf("modelArgs = %v, want -hf %s:Q5_K_M (canonical token)", rm.modelArgs, repo)
			}
		})
	}
}

// An ambiguous --quant (matching more than one quantization) errors rather than
// forwarding a partial tag that the engine would resolve unpredictably.
func TestResolveRawQuantAmbiguous(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	repo := "org/qwen3-gguf"
	srv := multiQuantGGUFServer(t, repo, []string{"m-Q4_K_M.gguf", "m-Q5_K_M.gguf"}, qwen3GGUFHeader(t))
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	_, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "K_M", repo)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v, want an ambiguity error for --quant K_M", err)
	}
}

// An unknown --quant errors and lists the repo's available quantizations.
func TestResolveRawQuantUnknown(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	repo := "org/qwen3-gguf"
	srv := multiQuantGGUFServer(t, repo, []string{"m-Q4_K_M.gguf", "m-Q8_0.gguf"}, qwen3GGUFHeader(t))
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	_, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "Q3_K_S", repo)
	if err == nil {
		t.Fatal("expected an error for an unavailable quant")
	}
	for _, want := range []string{"Q3_K_S", "Q4_K_M", "Q8_0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// --quant on something that is not a multi-quant repo (here a single local .gguf)
// is a clear error rather than a silent no-op.
func TestResolveRawQuantOnNonRepo(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	path := filepath.Join(t.TempDir(), "single.gguf")
	if err := os.WriteFile(path, qwen3GGUFHeader(t), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "Q4_K_M", path)
	if err == nil || !strings.Contains(err.Error(), "no selectable quantizations") {
		t.Errorf("error = %v, want no-selectable-quantizations message", err)
	}
}

// withCapacity overrides the detected host capacity (as GPU VRAM) for the duration
// of a test so the fit gate is deterministic without real hardware.
func withCapacity(t *testing.T, bytes int64) {
	t.Helper()
	old := detectCapacity
	detectCapacity = func() (int64, bool) { return bytes, true }
	t.Cleanup(func() { detectCapacity = old })
}

// An unsupported architecture is refused before any weight download, with a
// message naming the arch and the contribution pointer (M8 Phase 3, ADR-0015 3c).
func TestResolveRawRefusesUnsupportedArch(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	withCapacity(t, 80<<30) // roomy: the arch gate, not fit, must be what fires
	srv := sizedRepoServer(t, "org/exotic", "FooBarForCausalLM", "foobar", 1<<30)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	_, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "org/exotic")
	if err == nil {
		t.Fatal("expected a refusal for an unsupported architecture")
	}
	for _, want := range []string{"cannot serve", "FooBarForCausalLM", "open a PR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A model that exceeds the host's memory is refused before download, with the
// sizing shortfall.
func TestResolveRawRefusesOversized(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	withCapacity(t, 2<<30) // 2 GiB VRAM
	srv := sizedRepoServer(t, "org/big", "Qwen2ForCausalLM", "qwen2", 40<<30)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	_, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "org/big")
	if err == nil {
		t.Fatal("expected a refusal for an oversized model")
	}
	for _, want := range []string{"needs", "VRAM"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A loadable, fitting known-family model passes the gate and is auto-configured —
// the gate does not regress the P8.2 path.
func TestResolveRawGatePassesKnownFamily(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	withCapacity(t, 80<<30)
	srv := sizedRepoServer(t, "org/q2", "Qwen2ForCausalLM", "qwen2", 8<<30)
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "org/q2")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !containsPair(rm.engineArgs, "--tool-call-parser", "hermes") {
		t.Errorf("a fitting known family should still be auto-configured: %v", rm.engineArgs)
	}
}

// multiQuantSizedServer serves an HF GGUF repo whose listing reports a size per
// quant file, so the fit gate can be exercised against the *selected* quant.
func multiQuantSizedServer(t *testing.T, repo string, sizes map[string]int64, header []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/"+repo, func(w http.ResponseWriter, _ *http.Request) {
		parts := make([]string, 0, len(sizes))
		for f, sz := range sizes {
			parts = append(parts, fmt.Sprintf(`{"rfilename":%q,"size":%d}`, f, sz))
		}
		_, _ = fmt.Fprintf(w, `{"siblings":[%s]}`, strings.Join(parts, ","))
	})
	mux.HandleFunc("/"+repo+"/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	for f := range sizes {
		mux.HandleFunc("/"+repo+"/resolve/main/"+f, func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, f, time.Time{}, bytes.NewReader(header))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Regression for the P8.3 review --quant fit-gate bug: the default quant fits but
// an explicitly requested larger quant does not — the gate must weigh the selected
// quant's size, not the inspect-time default.
func TestResolveRawQuantFitWeighsSelected(t *testing.T) {
	cat, _ := catalog.Load()
	repo := "org/qwen3-gguf"
	sizes := map[string]int64{"m-Q4_K_M.gguf": 4 << 30, "m-Q8_0.gguf": 40 << 30}
	withCapacity(t, 10<<30) // fits Q4_K_M (~4.8 GiB padded), not Q8_0 (~48 GiB)

	// No --quant: the default Q4_K_M is weighed and fits.
	st := store.New(t.TempDir())
	srv := multiQuantSizedServer(t, repo, sizes, qwen3GGUFHeader(t))
	t.Setenv("ATLAS_HF_ENDPOINT", srv.URL)
	if _, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", repo); err != nil {
		t.Fatalf("default quant should fit: %v", err)
	}

	// --quant Q8_0: the larger selected quant is weighed and is refused.
	st2 := store.New(t.TempDir())
	srv2 := multiQuantSizedServer(t, repo, sizes, qwen3GGUFHeader(t))
	t.Setenv("ATLAS_HF_ENDPOINT", srv2.URL)
	_, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st2, cat, t.TempDir(), "Q8_0", repo)
	if err == nil || !strings.Contains(err.Error(), "needs") {
		t.Fatalf("a too-large --quant must be refused by the fit gate, got %v", err)
	}
}

// qwen3GGUFHeader hand-encodes a minimal GGUF header naming
// general.architecture=qwen3, enough for resolution to classify the family.
func qwen3GGUFHeader(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	putU32(&b, 3) // version
	putU64(&b, 0) // tensor count
	putU64(&b, 1) // kv count
	putStr(&b, "general.architecture")
	putU32(&b, 8) // ggufString type
	putStr(&b, "qwen3")
	return b.Bytes()
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func TestResolveModelEngineMismatch(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	// qwen3.5-35b-a3b is a vLLM catalog entry; resolving it under llamacpp errors.
	if _, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", "qwen3.5-35b-a3b"); err == nil {
		t.Fatal("expected engine-mismatch error")
	}
}

func TestResolveModelHFServesUnderName(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, t.TempDir(), "", "qwen3.5-35b-a3b")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if rm.served != "qwen3.5-35b-a3b" {
		t.Errorf("served = %q", rm.served)
	}
	if !reflect.DeepEqual(rm.modelArgs, []string{"Qwen/Qwen3.5-35B-A3B"}) {
		t.Errorf("modelArgs = %v", rm.modelArgs)
	}
	// The logical name is wired through as --served-model-name so clients can
	// address it regardless of the repo id.
	if !containsPair(rm.engineArgs, "--served-model-name", "qwen3.5-35b-a3b") {
		t.Errorf("engineArgs missing served-model-name: %v", rm.engineArgs)
	}
	if rm.ctxHint != 262144 {
		t.Errorf("ctxHint = %d", rm.ctxHint)
	}
}

// TestResolveModelGGUFAutoPulls stands up a fake catalog whose gguf URL points
// at an httptest server, then resolves it and asserts the blob was pulled into
// the store and the model args point at it.
func TestResolveModelGGUFAutoPulls(t *testing.T) {
	body := []byte("tiny gguf bytes")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cat := testCatalog(t, "small-test", srv.URL+"/m.gguf", hex.EncodeToString(sum[:]))
	st := store.New(t.TempDir())

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, t.TempDir(), "", "small-test")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if rm.served != "small-test" {
		t.Errorf("served = %q", rm.served)
	}
	if len(rm.modelArgs) != 2 || rm.modelArgs[0] != "-m" {
		t.Fatalf("modelArgs = %v", rm.modelArgs)
	}
	got, err := os.ReadFile(rm.modelArgs[1])
	if err != nil {
		t.Fatalf("read pulled blob: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("pulled blob = %q", got)
	}
	if !st.Has("small-test") {
		t.Error("store.Has = false after resolve")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:        "512 B",
		1024:       "1.0 KiB",
		1117320736: "1.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func containsPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// testCatalog writes a one-entry gguf catalog to a temp dir and loads it via the
// same parser the embedded catalog uses, so tests exercise resolution without
// depending on the shipped (large, network-bound) entries.
func testCatalog(t *testing.T, name, url, sha string) *catalog.Catalog {
	t.Helper()
	doc := "models:\n" +
		"  - name: " + name + "\n" +
		"    engine: llamacpp\n" +
		"    tier: haiku\n" +
		"    context_window: 4096\n" +
		"    source:\n" +
		"      type: gguf\n" +
		"      url: " + url + "\n" +
		"      sha256: " + sha + "\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return c
}
