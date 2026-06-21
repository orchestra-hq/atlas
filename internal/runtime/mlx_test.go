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
	// Pre-seed the uv binary so EnsureUv short-circuits (no download).
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")
	if err := os.MkdirAll(filepath.Dir(uvBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	venv := filepath.Join(dir, "mlx", MLXVersion, "venv")
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

	got, err := p.EnsureMLX(context.Background(), "darwin", "arm64")
	if err != nil {
		t.Fatalf("EnsureMLX: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
	if len(calls) != 2 {
		t.Fatalf("uv invocations = %v, want venv then pip install", calls)
	}
	// uv venv <venv> --python 3.12
	if c := calls[0]; c[0] != uvBin || c[1] != "venv" || c[2] != venv || c[3] != "--python" || c[4] != mlxPython {
		t.Errorf("venv call = %v", c)
	}
	// uv pip install --python <venv> mlx-lm==<version>
	pip := calls[1]
	if pip[0] != uvBin || pip[1] != "pip" || pip[2] != "install" || pip[len(pip)-1] != "mlx-lm=="+MLXVersion {
		t.Errorf("install call = %v", pip)
	}
}
