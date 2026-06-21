package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/orchestra-hq/atlas/internal/worker"
)

func TestModelArgs(t *testing.T) {
	dir := t.TempDir()
	localGGUF := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(localGGUF, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	noExtFile := filepath.Join(dir, "weights")
	if err := os.WriteFile(noExtFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		engine worker.Engine
		model  string
		want   []string
	}{
		{"llamacpp local gguf path", worker.EngineLlamaCpp, localGGUF, []string{"-m", localGGUF}},
		{"llamacpp existing file without extension", worker.EngineLlamaCpp, noExtFile, []string{"-m", noExtFile}},
		{"llamacpp gguf suffix not on disk", worker.EngineLlamaCpp, "/not/here/model.gguf", []string{"-m", "/not/here/model.gguf"}},
		{"llamacpp hf spec", worker.EngineLlamaCpp, "ggml-org/Qwen2.5-0.5B-Instruct-GGUF", []string{"-hf", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"}},
		{"llamacpp hf spec with quant", worker.EngineLlamaCpp, "unsloth/Qwen3-GGUF:Q4_K_M", []string{"-hf", "unsloth/Qwen3-GGUF:Q4_K_M"}},
		{"vllm hf repo positional", worker.EngineVLLM, "Qwen/Qwen2.5-1.5B-Instruct", []string{"Qwen/Qwen2.5-1.5B-Instruct"}},
		{"vllm local path positional", worker.EngineVLLM, localGGUF, []string{localGGUF}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelArgs(tc.engine, tc.model); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("modelArgs(%q, %q) = %v, want %v", tc.engine, tc.model, got, tc.want)
			}
		})
	}
}

func TestModelDisplayName(t *testing.T) {
	dir := t.TempDir()
	localGGUF := filepath.Join(dir, "Qwen2.5-0.5B.gguf")
	if err := os.WriteFile(localGGUF, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		engine worker.Engine
		model  string
		want   string
	}{
		{worker.EngineLlamaCpp, localGGUF, "Qwen2.5-0.5B"},
		{worker.EngineLlamaCpp, "/not/here/foo.gguf", "foo"},
		{worker.EngineLlamaCpp, "ggml-org/Qwen2.5-0.5B-Instruct-GGUF", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"},
		// vLLM serves under the ref as-is, so a local path is not stripped.
		{worker.EngineVLLM, "Qwen/Qwen2.5-1.5B-Instruct", "Qwen/Qwen2.5-1.5B-Instruct"},
		{worker.EngineVLLM, localGGUF, localGGUF},
	}
	for _, tc := range tests {
		if got := modelDisplayName(tc.engine, tc.model); got != tc.want {
			t.Errorf("modelDisplayName(%q, %q) = %q, want %q", tc.engine, tc.model, got, tc.want)
		}
	}
}

func TestParseEngine(t *testing.T) {
	for _, s := range []string{"llamacpp", "vllm", "mlx", "sglang"} {
		if _, err := parseEngine(s); err != nil {
			t.Errorf("parseEngine(%q) errored: %v", s, err)
		}
	}
	if _, err := parseEngine("nonesuch"); err == nil {
		t.Error("expected error for unknown engine")
	}
}
