package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeUvArchive builds a gzipped tar shaped like a uv release: a single
// top-level uv-<target>/ directory containing the uv and uvx binaries.
func fakeUvArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	entries := []struct {
		name string
		mode int64
		body string
	}{
		{"uv-test/", 0o755 | 0o40000, ""},
		{"uv-test/uv", 0o755, "#!/bin/sh\necho fake uv\n"},
		{"uv-test/uvx", 0o755, "#!/bin/sh\necho fake uvx\n"},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body))}
		if strings.HasSuffix(e.name, "/") {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEnsureUvDownloadsAndIsIdempotent(t *testing.T) {
	archive := fakeUvArchive(t)
	srv, hits := serveArchive(t, archive)

	dir := t.TempDir()
	p := &Provisioner{Dir: dir, Client: srv.Client(), BaseURL: srv.URL}
	// Register a synthetic platform whose digest matches the fake archive, so
	// EnsureUv exercises the real download/verify/extract path.
	uvAssets["test/test"] = asset{name: "uv-test.tar.gz", sha256: sha256Hex(archive)}
	t.Cleanup(func() { delete(uvAssets, "test/test") })

	bin, err := p.EnsureUv(context.Background(), "test", "test")
	if err != nil {
		t.Fatalf("EnsureUv: %v", err)
	}
	wantBin := filepath.Join(dir, "uv", UvVersion, "uv")
	if bin != wantBin {
		t.Errorf("bin = %q, want %q", bin, wantBin)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat uv: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("uv not executable: %v", info.Mode())
	}

	// Idempotent: a second call does not re-download.
	if _, err := p.EnsureUv(context.Background(), "test", "test"); err != nil {
		t.Fatalf("second EnsureUv: %v", err)
	}
	if *hits != 1 {
		t.Errorf("download hits = %d, want 1 (second call cached)", *hits)
	}
}

func TestEnsureUvUnsupportedPlatform(t *testing.T) {
	p := &Provisioner{Dir: t.TempDir()}
	if _, err := p.EnsureUv(context.Background(), "plan9", "sparc"); err == nil {
		t.Fatal("expected error for unsupported platform")
	}
}

func TestEnsureUvKnownPlatforms(t *testing.T) {
	for _, plat := range []string{"darwin/arm64", "darwin/amd64", "linux/arm64", "linux/amd64"} {
		if _, ok := uvAssets[plat]; !ok {
			t.Errorf("missing pinned uv asset for %s", plat)
		}
	}
}

func TestEnsureVLLMIdempotent(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "vllm", VLLMVersion, "venv", "bin", "vllm")
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
	got, err := p.EnsureVLLM(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureVLLM: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
}

func TestEnsureVLLMProvisions(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed the uv binary so EnsureUv short-circuits (no download).
	uvBin := filepath.Join(dir, "uv", UvVersion, "uv")
	if err := os.MkdirAll(filepath.Dir(uvBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	venv := filepath.Join(dir, "vllm", VLLMVersion, "venv")
	binPath := filepath.Join(venv, "bin", "vllm")
	var calls [][]string
	p := &Provisioner{Dir: dir, run: func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		// The pip install is what materializes the vllm entrypoint.
		if len(args) > 0 && args[0] == "pip" {
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755)
		}
		return nil
	}}

	got, err := p.EnsureVLLM(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureVLLM: %v", err)
	}
	if got != binPath {
		t.Errorf("bin = %q, want %q", got, binPath)
	}
	if len(calls) != 2 {
		t.Fatalf("uv invocations = %v, want venv then pip install", calls)
	}
	// uv venv <venv> --python 3.12
	if c := calls[0]; c[0] != uvBin || c[1] != "venv" || c[2] != venv || c[3] != "--python" || c[4] != vllmPython {
		t.Errorf("venv call = %v", c)
	}
	// uv pip install --python <venv> vllm==<version>
	pip := calls[1]
	if pip[0] != uvBin || pip[1] != "pip" || pip[2] != "install" || pip[len(pip)-1] != "vllm=="+VLLMVersion {
		t.Errorf("install call = %v", pip)
	}
}
