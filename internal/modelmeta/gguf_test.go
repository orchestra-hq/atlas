package modelmeta

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- GGUF fixture builder ---

type kvEntry struct {
	key string
	typ uint32
	s   string   // ggufString
	u   uint32   // ggufUint32
	arr []string // ggufArray of strings
}

func kvString(k, v string) kvEntry { return kvEntry{key: k, typ: ggufString, s: v} }
func kvU32(k string, v uint32) kvEntry {
	return kvEntry{key: k, typ: ggufUint32, u: v}
}

func kvStrArray(k string, items []string) kvEntry {
	return kvEntry{key: k, typ: ggufArray, arr: items}
}

func writeU32(b *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}

func writeU64(b *bytes.Buffer, v uint64) {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	b.Write(tmp[:])
}

func writeStr(b *bytes.Buffer, s string) {
	writeU64(b, uint64(len(s)))
	b.WriteString(s)
}

func buildGGUF(entries []kvEntry) []byte {
	var b bytes.Buffer
	b.WriteString("GGUF")
	writeU32(&b, 3)                    // version
	writeU64(&b, 0)                    // tensor count
	writeU64(&b, uint64(len(entries))) // kv count
	for _, e := range entries {
		writeStr(&b, e.key)
		writeU32(&b, e.typ)
		switch e.typ {
		case ggufString:
			writeStr(&b, e.s)
		case ggufUint32:
			writeU32(&b, e.u)
		case ggufArray:
			writeU32(&b, ggufString)
			writeU64(&b, uint64(len(e.arr)))
			for _, it := range e.arr {
				writeStr(&b, it)
			}
		}
	}
	return b.Bytes()
}

func qwenGGUF() []byte {
	return buildGGUF([]kvEntry{
		kvString("general.architecture", "qwen2"),
		// An array KV before the wanted keys exercises array-skipping cursor advance.
		kvStrArray("tokenizer.ggml.tokens", []string{"a", "b", "c"}),
		kvU32("qwen2.context_length", 32768),
		kvString("tokenizer.chat_template", "{{ messages }}"),
	})
}

// --- parser ---

func TestParseGGUFHeader(t *testing.T) {
	meta, err := parseGGUFHeader(qwenGGUF())
	if err != nil {
		t.Fatalf("parseGGUFHeader: %v", err)
	}
	if meta.architecture != "qwen2" {
		t.Errorf("architecture = %q, want qwen2", meta.architecture)
	}
	if meta.contextWindow != 32768 {
		t.Errorf("context = %d, want 32768", meta.contextWindow)
	}
	if !meta.hasChatTemplate {
		t.Error("hasChatTemplate = false, want true")
	}
}

// Negative control: no chat_template key → hasChatTemplate false.
func TestParseGGUFNoTemplate(t *testing.T) {
	data := buildGGUF([]kvEntry{
		kvString("general.architecture", "llama"),
		kvU32("llama.context_length", 4096),
	})
	meta, err := parseGGUFHeader(data)
	if err != nil {
		t.Fatalf("parseGGUFHeader: %v", err)
	}
	if meta.hasChatTemplate {
		t.Error("hasChatTemplate = true, want false")
	}
}

func TestParseGGUFBadMagic(t *testing.T) {
	data := qwenGGUF()
	data[0] = 'X'
	if _, err := parseGGUFHeader(data); !errors.Is(err, errNotGGUF) {
		t.Errorf("err = %v, want errNotGGUF", err)
	}
}

func TestParseGGUFShort(t *testing.T) {
	data := qwenGGUF()
	if _, err := parseGGUFHeader(data[:20]); !errors.Is(err, errShortHeader) {
		t.Errorf("err = %v, want errShortHeader", err)
	}
}

// A crafted huge string length must not panic the slice bound check (the length
// is attacker-controlled from the file). It should fail cleanly.
func TestParseGGUFHugeLengthNoPanic(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GGUF")
	writeU32(&b, 3) // version
	writeU64(&b, 0) // tensor count
	writeU64(&b, 1) // kv count
	// key with an absurd length 0x7FFFFFFFFFFFFFF5 → must be rejected, not panic.
	writeU64(&b, 0x7FFFFFFFFFFFFFF5)
	b.WriteString("general.architecture")
	if _, err := parseGGUFHeader(b.Bytes()); err == nil {
		t.Fatal("expected an error for an absurd field length")
	}
}

// --- local file ---

func TestInspectGGUFLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-Q4_K_M.gguf")
	if err := os.WriteFile(path, qwenGGUF(), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Inspect(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.Capabilities.Format != FormatGGUF {
		t.Errorf("format = %q, want gguf", res.Capabilities.Format)
	}
	if res.Capabilities.Architecture != "qwen2" || res.Capabilities.ContextWindow != 32768 {
		t.Errorf("arch/ctx = %q/%d", res.Capabilities.Architecture, res.Capabilities.ContextWindow)
	}
	if !res.Capabilities.HasChatTemplate {
		t.Error("HasChatTemplate = false")
	}
	if len(res.Capabilities.Engines) == 0 || res.Capabilities.Engines[0] != "llamacpp" {
		t.Errorf("engines = %v, want [llamacpp]", res.Capabilities.Engines)
	}
}

// --- ranged URL: only a bounded range is requested, no full GET ---

func TestInspectGGUFURLRanged(t *testing.T) {
	full := qwenGGUF()
	var rangeSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeSeen = r.Header.Get("Range")
		http.ServeContent(w, r, "model.gguf", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)

	res, err := Inspect(context.Background(), srv.URL+"/model.gguf", Options{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.Capabilities.Architecture != "qwen2" {
		t.Errorf("arch = %q", res.Capabilities.Architecture)
	}
	if !strings.HasPrefix(rangeSeen, "bytes=0-") {
		t.Errorf("Range header = %q, want a bounded bytes=0- request (no full GET)", rangeSeen)
	}
}

// --- HF multi-quant GGUF repo ---

func TestInspectGGUFRepo(t *testing.T) {
	full := qwenGGUF()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[{"rfilename":"README.md"},{"rfilename":"model-Q8_0.gguf"},{"rfilename":"model-Q4_K_M.gguf"}]}`))
	})
	// config.json 404s (not a transformers repo) → repo path kicks in.
	mux.HandleFunc("/org/repo/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/org/repo/resolve/main/model-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "model.gguf", time.Time{}, bytes.NewReader(full))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := Inspect(context.Background(), "org/repo", Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	c := res.Capabilities
	if c.Format != FormatGGUF {
		t.Errorf("format = %q, want gguf", c.Format)
	}
	if !strings.Contains(c.Selected, "Q4_K_M") {
		t.Errorf("selected = %q, want the Q4_K_M default", c.Selected)
	}
	if len(c.Files) != 2 {
		t.Errorf("files = %v, want the 2 gguf files", c.Files)
	}
	if c.Architecture != "qwen2" {
		t.Errorf("arch = %q", c.Architecture)
	}
}

func TestInspectGGUFRepoWeightBytes(t *testing.T) {
	full := qwenGGUF()
	mux := http.NewServeMux()
	// The listing reports the selected quant's size; WeightBytes takes that one
	// (the served quant), not the sum across quantizations.
	mux.HandleFunc("/api/models/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[
			{"rfilename":"model-Q8_0.gguf","size":8000000000},
			{"rfilename":"model-Q4_K_M.gguf","size":4000000000}
		]}`))
	})
	mux.HandleFunc("/org/repo/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/org/repo/resolve/main/model-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "model.gguf", time.Time{}, bytes.NewReader(full))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := Inspect(context.Background(), "org/repo", Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got, want := res.Capabilities.WeightBytes, int64(4_000_000_000); got != want {
		t.Errorf("WeightBytes = %d, want %d (the selected Q4_K_M quant)", got, want)
	}
}

func TestPickQuant(t *testing.T) {
	if got := pickQuant([]string{"m-Q8_0.gguf", "m-Q4_K_M.gguf"}); !strings.Contains(got, "Q4_K_M") {
		t.Errorf("pickQuant = %q, want Q4_K_M", got)
	}
	if got := pickQuant([]string{"a.gguf", "b.gguf"}); got != "a.gguf" {
		t.Errorf("pickQuant = %q, want first when no Q4_K_M", got)
	}
}
