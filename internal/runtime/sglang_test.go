package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSGLangIdempotent(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "sglang", SGLangVersion, "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A provisioned venv must short-circuit: the runner is never invoked.
	p := &Provisioner{Dir: dir, run: func(context.Context, string, ...string) error {
		t.Fatal("runner must not be called when the venv already exists")
		return nil
	}}
	got, err := p.EnsureSGLang(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureSGLang: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
}

func TestEnsureSGLangProvisions(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed the uv binary so EnsureUv short-circuits (no download).
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")
	if err := os.MkdirAll(filepath.Dir(uvBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	venv := filepath.Join(dir, "sglang", SGLangVersion, "venv")
	binPath := filepath.Join(venv, "bin", "python")
	var calls [][]string
	p := &Provisioner{Dir: dir, run: func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		// The venv create is what materializes the python interpreter.
		if len(args) > 0 && args[0] == "venv" {
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755)
		}
		return nil
	}}

	got, err := p.EnsureSGLang(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureSGLang: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
	if len(calls) != 2 {
		t.Fatalf("uv invocations = %v, want venv then pip install", calls)
	}
	// uv venv <venv> --python 3.12
	if c := calls[0]; c[0] != uvBin || c[1] != "venv" || c[2] != venv || c[3] != "--python" || c[4] != sglangPython {
		t.Errorf("venv call = %v", c)
	}
	// uv pip install --python <venv> sglang[all]==<version>
	pip := calls[1]
	if pip[0] != uvBin || pip[1] != "pip" || pip[2] != "install" || pip[len(pip)-1] != "sglang[all]=="+SGLangVersion {
		t.Errorf("install call = %v", pip)
	}
}
