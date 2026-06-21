package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureMLXRequiresAppleSilicon(t *testing.T) {
	p := &Provisioner{Dir: t.TempDir(), run: func(context.Context, string, ...string) error {
		t.Fatal("runner must not be called on an unsupported platform")
		return nil
	}}
	for _, plat := range [][2]string{{"linux", "amd64"}, {"darwin", "amd64"}, {"linux", "arm64"}} {
		if _, err := p.EnsureMLX(context.Background(), plat[0], plat[1]); err == nil {
			t.Errorf("EnsureMLX(%s/%s) = nil error, want a non-Apple-Silicon rejection", plat[0], plat[1])
		}
	}
}

func TestEnsureMLXIdempotent(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "mlx", MLXVersion, "venv", "bin", "python")
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
	got, err := p.EnsureMLX(context.Background(), "darwin", "arm64")
	if err != nil {
		t.Fatalf("EnsureMLX: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
}

func TestEnsureMLXProvisions(t *testing.T) {
	dir := t.TempDir()
	seedUv(t, dir)
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")

	var calls [][]string
	p := &Provisioner{Dir: dir, run: venvFakeRunner(&calls)}
	got, err := p.EnsureMLX(context.Background(), "darwin", "arm64")
	if err != nil {
		t.Fatalf("EnsureMLX: %v", err)
	}
	wantBin := filepath.Join(dir, "mlx", MLXVersion, "venv", "bin", "python")
	if got != wantBin {
		t.Errorf("bin = %q, want %q", got, wantBin)
	}
	venvCallAsserts(t, calls, dir, uvBin, "mlx", MLXVersion, mlxPython, "mlx-lm=="+MLXVersion)
}
