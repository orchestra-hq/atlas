package modelmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

// fakeHF serves fixture metadata files the way Hugging Face's resolve endpoint
// does, counting hits per file and recording the last Authorization header so
// tests can assert auth and (in the cli package) caching behaviour.
type fakeHF struct {
	files    map[string]fileResp
	hits     map[string]int
	lastAuth string
}

type fileResp struct {
	status int // 0 -> 200
	body   string
}

func newFakeHF(files map[string]fileResp) *fakeHF {
	return &fakeHF{files: files, hits: map[string]int{}}
}

func (f *fakeHF) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		name := path.Base(r.URL.Path)
		f.hits[name]++
		resp, ok := f.files[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if resp.status != 0 && resp.status != http.StatusOK {
			w.WriteHeader(resp.status)
			return
		}
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	fxConfigQwen      = `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","max_position_embeddings":32768}`
	fxConfigRope      = `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","max_position_embeddings":32768,"rope_scaling":{"rope_type":"yarn","factor":4.0}}`
	fxTokenizer       = `{"chat_template":"{% for m in messages %}{{ m.content }}{% endfor %}"}`
	fxTokenizerNoTmpl = `{"bos_token":"<s>"}`
	fxGeneration      = `{"temperature":0.7,"top_p":0.8}`
)

func TestInspectHFWeightBytes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/m/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fxConfigQwen))
	})
	// The blobs=true listing carries sizes; safetensors shards sum, other files are
	// ignored. The second shard reports its size under lfs (weight files are LFS).
	mux.HandleFunc("/api/models/org/m", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[
			{"rfilename":"config.json","size":1024},
			{"rfilename":"model-00001-of-00002.safetensors","size":3000000000},
			{"rfilename":"model-00002-of-00002.safetensors","lfs":{"size":2000000000}},
			{"rfilename":"README.md","size":4096}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := Inspect(context.Background(), "org/m", Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got, want := res.Capabilities.WeightBytes, int64(5_000_000_000); got != want {
		t.Errorf("WeightBytes = %d, want %d (sum of safetensors shards only)", got, want)
	}
}

func TestInspectHFDerivesPlan(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{
		"config.json":            {body: fxConfigQwen},
		"tokenizer_config.json":  {body: fxTokenizer},
		"generation_config.json": {body: fxGeneration},
	})
	srv := hf.server(t)

	res, err := Inspect(context.Background(), "org/qwen2.5-7b", Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	c := res.Capabilities
	if c.Format != FormatSafetensors {
		t.Errorf("format = %q, want safetensors", c.Format)
	}
	if c.Architecture != "Qwen2ForCausalLM" || c.ModelType != "qwen2" {
		t.Errorf("arch/type = %q/%q", c.Architecture, c.ModelType)
	}
	if c.ContextWindow != 32768 {
		t.Errorf("context = %d, want 32768", c.ContextWindow)
	}
	if !c.HasChatTemplate {
		t.Error("HasChatTemplate = false, want true")
	}
	if c.Sampling.Temperature == nil || *c.Sampling.Temperature != 0.7 {
		t.Errorf("temperature = %v, want 0.7", c.Sampling.Temperature)
	}
	if c.Sampling.TopP == nil || *c.Sampling.TopP != 0.8 {
		t.Errorf("top_p = %v, want 0.8", c.Sampling.TopP)
	}
	if len(c.Engines) == 0 {
		t.Error("no candidate engines")
	}
	if res.Verdict.Conclusion != ConclusionInspected {
		t.Errorf("conclusion = %q", res.Verdict.Conclusion)
	}
	if res.Verdict.Engine != c.Engines[0] {
		t.Errorf("verdict engine = %q, want %q", res.Verdict.Engine, c.Engines[0])
	}
	if res.Verdict.Family != "qwen2" {
		t.Errorf("family = %q, want qwen2 (classified, no longer pending)", res.Verdict.Family)
	}
	// Loadable is decided here (host-independent: qwen2 is a supported arch); Fits
	// stays Pending in the record (host-dependent, filled live by the consumer).
	if res.Verdict.Loadable != "yes" {
		t.Errorf("loadable = %q, want yes", res.Verdict.Loadable)
	}
	if res.Verdict.Fits != Pending {
		t.Errorf("fits = %q, want pending in the record", res.Verdict.Fits)
	}
}

func TestInspectHFRopeScaling(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{"config.json": {body: fxConfigRope}})
	res, err := Inspect(context.Background(), "org/m", Options{Endpoint: hf.server(t).URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := res.Capabilities.RopeScaling; got != "yarn x4" {
		t.Errorf("rope = %q, want \"yarn x4\"", got)
	}
}

// Negative control: drop chat_template and HasChatTemplate must flip to false.
func TestInspectHFNoChatTemplate(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{
		"config.json":           {body: fxConfigQwen},
		"tokenizer_config.json": {body: fxTokenizerNoTmpl},
	})
	res, err := Inspect(context.Background(), "org/m", Options{Endpoint: hf.server(t).URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.Capabilities.HasChatTemplate {
		t.Error("HasChatTemplate = true, want false")
	}
}

// Only config.json present: optional files 404, no crash, fields stay zero.
func TestInspectHFOptionalFilesAbsent(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{"config.json": {body: fxConfigQwen}})
	res, err := Inspect(context.Background(), "org/m", Options{Endpoint: hf.server(t).URL})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if res.Capabilities.HasChatTemplate {
		t.Error("HasChatTemplate should be false when tokenizer_config is absent")
	}
	if res.Capabilities.Sampling.Temperature != nil || res.Capabilities.Sampling.TopP != nil {
		t.Error("Sampling should be empty when generation_config is absent")
	}
}

// A repo that exists (its listing returns) but holds neither config.json nor any
// .gguf file is not something Atlas can serve — the error says so.
func TestInspectHFUnrecognizedRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/repo/resolve/main/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/models/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[{"rfilename":"README.md"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := Inspect(context.Background(), "org/repo", Options{Endpoint: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "not a recognized transformers or GGUF repo") {
		t.Errorf("error = %v, want unrecognized-repo message", err)
	}
}

func TestInspectHFGatedRepo(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{"config.json": {status: http.StatusUnauthorized}})
	srv := hf.server(t)
	_, err := Inspect(context.Background(), "org/gated", Options{Endpoint: srv.URL})
	if err == nil {
		t.Fatal("expected error for gated repo")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") || !strings.Contains(err.Error(), "gated") {
		t.Errorf("error = %v, want HF_TOKEN/gated remedy", err)
	}
}

func TestInspectHFSendsToken(t *testing.T) {
	hf := newFakeHF(map[string]fileResp{"config.json": {body: fxConfigQwen}})
	srv := hf.server(t)
	if _, err := Inspect(context.Background(), "org/m", Options{Endpoint: srv.URL, Token: "secret"}); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if hf.lastAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want \"Bearer secret\"", hf.lastAuth)
	}
}
