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
	seedUv(t, dir)
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")

	var calls [][]string
	p := &Provisioner{Dir: dir, run: venvFakeRunner(&calls)}
	got, err := p.EnsureSGLang(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureSGLang: %v", err)
	}
	wantBin := filepath.Join(dir, "sglang", SGLangVersion, "venv", "bin", "python")
	if got != wantBin {
		t.Errorf("bin = %q, want %q", got, wantBin)
	}
	venvCallAsserts(t, calls, dir, uvBin, "sglang", SGLangVersion, sglangPython, "sglang[all]=="+SGLangVersion)

	// sglang[all] pulls a prerelease-only flash-attn-4, so the install must allow
	// prereleases or uv's resolution fails (see EnsureSGLang).
	pip := calls[1]
	var sawPrerelease bool
	for _, a := range pip {
		if a == "--prerelease=allow" {
			sawPrerelease = true
		}
	}
	if !sawPrerelease {
		t.Errorf("sglang install call = %v, want --prerelease=allow", pip)
	}
}
