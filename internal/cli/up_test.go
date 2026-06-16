package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
		name  string
		model string
		want  []string
	}{
		{"local gguf path", localGGUF, []string{"-m", localGGUF}},
		{"existing file without extension", noExtFile, []string{"-m", noExtFile}},
		{"gguf suffix not on disk", "/not/here/model.gguf", []string{"-m", "/not/here/model.gguf"}},
		{"hf spec", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF", []string{"-hf", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"}},
		{"hf spec with quant", "unsloth/Qwen3-GGUF:Q4_K_M", []string{"-hf", "unsloth/Qwen3-GGUF:Q4_K_M"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelArgs(tc.model); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("modelArgs(%q) = %v, want %v", tc.model, got, tc.want)
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
		model string
		want  string
	}{
		{localGGUF, "Qwen2.5-0.5B"},
		{"/not/here/foo.gguf", "foo"},
		{"ggml-org/Qwen2.5-0.5B-Instruct-GGUF", "ggml-org/Qwen2.5-0.5B-Instruct-GGUF"},
	}
	for _, tc := range tests {
		if got := modelDisplayName(tc.model); got != tc.want {
			t.Errorf("modelDisplayName(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
