package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// runProvision executes `atlas runtime provision` with the given args, capturing
// output. It never reaches the network for an invalid engine (validation fails
// first), which is what these tests exercise.
func runProvision(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRuntimeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"provision"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestRuntimeProvisionRejectsUnknownEngine(t *testing.T) {
	_, err := runProvision(t, "--engine", "tensorrt", "--state-dir", t.TempDir())
	if err == nil {
		t.Fatal("expected an error for an unknown engine")
	}
	if !strings.Contains(err.Error(), "invalid --engine") {
		t.Errorf("unexpected error: %v", err)
	}
}

// runRuntime executes an `atlas runtime` subcommand, capturing output.
func runRuntime(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRuntimeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRuntimeListMarksPinnedAndProvisioned(t *testing.T) {
	stateDir := t.TempDir()
	runtimes := filepath.Join(stateDir, "runtimes")
	// Provision an old vLLM version plus the currently-pinned one on disk.
	for _, v := range []string{"0.0.1-old", atlasruntime.VLLMVersion} {
		if err := os.MkdirAll(filepath.Join(runtimes, "vllm", v), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runRuntime(t, "list", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("runtime list: %v", err)
	}
	for _, want := range []string{"ENGINE", "PINNED", "vllm", "0.0.1-old", atlasruntime.VLLMVersion + " (pinned)", "mlx", "—"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestRuntimeUpgradePrunesOlderVersions(t *testing.T) {
	stateDir := t.TempDir()
	prov := &atlasruntime.Provisioner{Dir: filepath.Join(stateDir, "runtimes")}
	// Two stale versions plus the pinned one already provisioned, so the upgrade's
	// provision step is a no-op (pinned entrypoint present) and only --prune acts —
	// no network/uv needed.
	for _, v := range []string{"0.0.1", "0.0.2", atlasruntime.SGLangVersion} {
		bin := filepath.Join(prov.Dir, "sglang", v, "venv", "bin", "python")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runRuntime(t, "upgrade", "--engine", string(worker.EngineSGLang), "--prune", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("runtime upgrade: %v", err)
	}
	if !strings.Contains(out, "Pruned older sglang versions") {
		t.Errorf("expected prune output, got:\n%s", out)
	}
	left, _ := prov.ProvisionedVersions("sglang")
	if len(left) != 1 || left[0] != atlasruntime.SGLangVersion {
		t.Errorf("remaining versions = %v, want [%s]", left, atlasruntime.SGLangVersion)
	}
}
