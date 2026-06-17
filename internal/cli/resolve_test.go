package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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

	// A Hugging Face spec that is not a catalog name keeps the pre-catalog path.
	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, "ggml-org/Qwen2.5-0.5B-Instruct-GGUF")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if rm.served != "ggml-org/Qwen2.5-0.5B-Instruct-GGUF" {
		t.Errorf("served = %q", rm.served)
	}
	if !reflect.DeepEqual(rm.modelArgs, []string{"-hf", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"}) {
		t.Errorf("modelArgs = %v", rm.modelArgs)
	}
	if rm.ctxHint != 0 {
		t.Errorf("ctxHint = %d, want 0 for raw spec", rm.ctxHint)
	}
}

func TestResolveModelEngineMismatch(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	// qwen3.5-35b-a3b is a vLLM catalog entry; resolving it under llamacpp errors.
	if _, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, "qwen3.5-35b-a3b"); err == nil {
		t.Fatal("expected engine-mismatch error")
	}
}

func TestResolveModelHFServesUnderName(t *testing.T) {
	cat, _ := catalog.Load()
	st := store.New(t.TempDir())
	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineVLLM, st, cat, "qwen3.5-35b-a3b")
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

	rm, err := resolveModel(context.Background(), testCmd(), worker.EngineLlamaCpp, st, cat, "small-test")
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
