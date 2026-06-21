package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errInstall is a stand-in for a failed `uv pip install`.
var errInstall = errors.New("fake pip install failed")

// seedUv pre-seeds the pinned uv binary under dir so EnsureUv short-circuits (no
// download) and a venv test exercises only the venv provisioning path.
func seedUv(t *testing.T, dir string) {
	t.Helper()
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")
	if err := os.MkdirAll(filepath.Dir(uvBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// touchExe writes an executable stub at path, creating parents.
func touchExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// venvFakeRunner returns a fake uv runner that records its calls and materializes
// a venv the way real uv would: `uv venv <path>` creates the venv python, and
// `uv pip install --python <venv> <pkg>` creates the vllm console entrypoint. So
// both the python-launched engines (MLX/SGLang) and vLLM find their entrypoint in
// the staging venv. The run order mirrors ensureVenv: venv then pip.
func venvFakeRunner(calls *[][]string) func(context.Context, string, ...string) error {
	return func(_ context.Context, name string, args ...string) error {
		*calls = append(*calls, append([]string{name}, args...))
		if len(args) == 0 {
			return nil
		}
		switch args[0] {
		case "venv":
			venv := args[1] // uv venv <venv> --python X --relocatable
			return os.WriteFile(mkbin(venv, "python"), []byte("#!/bin/sh\n"), 0o755)
		case "pip":
			venv := args[3] // uv pip install --python <venv> <pkg>
			return os.WriteFile(mkbin(venv, "vllm"), []byte("#!/bin/sh\n"), 0o755)
		}
		return nil
	}
}

func mkbin(venv, name string) string {
	bin := filepath.Join(venv, "bin")
	_ = os.MkdirAll(bin, 0o755)
	return filepath.Join(bin, name)
}

// venvCallAsserts checks the shared shape of a venv provisioning run: a relocatable
// venv created in a staging dir (not the final dest), then a pinned pip install.
func venvCallAsserts(t *testing.T, calls [][]string, dir, uvBin, engine, version, python, pkg string) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("uv invocations = %v, want venv then pip install", calls)
	}
	venv := calls[0]
	if venv[0] != uvBin || venv[1] != "venv" || venv[3] != "--python" || venv[4] != python {
		t.Errorf("venv call = %v", venv)
	}
	if !contains(venv, "--relocatable") {
		t.Errorf("venv call missing --relocatable (needed so console scripts survive the swap): %v", venv)
	}
	// The venv must be created in a staging dir, then renamed into the final dest —
	// never built directly under <engine>/<version>, which would leave a partial
	// venv on a crash.
	finalDest := filepath.Join(dir, engine, version)
	if strings.HasPrefix(venv[2], finalDest) {
		t.Errorf("venv built directly in final dest %q, want a staging dir", venv[2])
	}
	pip := calls[1]
	if pip[0] != uvBin || pip[1] != "pip" || pip[2] != "install" || pip[len(pip)-1] != pkg {
		t.Errorf("install call = %v", pip)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestEnsureVenvStagingIsAtomic: a pip install that fails leaves no version dir at
// the final path (only a swept-away staging dir), so the next run re-provisions
// rather than trusting a partial venv.
func TestEnsureVenvStagingIsAtomic(t *testing.T) {
	dir := t.TempDir()
	seedUv(t, dir)

	failPip := func(_ context.Context, _ string, args ...string) error {
		if len(args) > 0 && args[0] == "venv" {
			return os.WriteFile(mkbin(args[1], "python"), []byte("#!/bin/sh\n"), 0o755)
		}
		return errInstall // pip install fails
	}
	p := &Provisioner{Dir: dir, run: failPip}
	if _, err := p.EnsureSGLang(context.Background(), "linux", "amd64"); err == nil {
		t.Fatal("expected EnsureSGLang to fail when pip install fails")
	}
	// No final version dir, and no leftover staging dir.
	if _, err := os.Stat(filepath.Join(dir, "sglang", SGLangVersion)); !os.IsNotExist(err) {
		t.Errorf("final version dir exists after a failed install (err=%v); want absent", err)
	}
	versions, _ := p.ProvisionedVersions("sglang")
	if len(versions) != 0 {
		t.Errorf("ProvisionedVersions = %v after failed install, want none", versions)
	}
}

// TestProvisionedVersionsAndPrune: Prune keeps the pinned version and removes the
// rest, the disk side of `atlas runtime upgrade --prune`.
func TestProvisionedVersionsAndPrune(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{Dir: dir}
	for _, v := range []string{"0.1.0", "0.2.0", "0.3.0"} {
		touchExe(t, filepath.Join(dir, "vllm", v, "venv", "bin", "vllm"))
	}
	got, err := p.ProvisionedVersions("vllm")
	if err != nil {
		t.Fatalf("ProvisionedVersions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ProvisionedVersions = %v, want 3", got)
	}

	removed, err := p.Prune("vllm", "0.3.0")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v, want 2 (kept 0.3.0)", removed)
	}
	left, _ := p.ProvisionedVersions("vllm")
	if len(left) != 1 || left[0] != "0.3.0" {
		t.Errorf("remaining versions = %v, want [0.3.0]", left)
	}

	// Pruning an engine with nothing provisioned is a no-op, not an error.
	if r, err := p.Prune("mlx", MLXVersion); err != nil || r != nil {
		t.Errorf("Prune(mlx) = %v, %v; want nil, nil", r, err)
	}
}
