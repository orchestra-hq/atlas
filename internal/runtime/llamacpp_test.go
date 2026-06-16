package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeArchive builds a gzipped tar shaped like a llama.cpp release: a single
// top-level llama-<tag>/ directory containing llama-server and a sibling lib.
func fakeArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	entries := []struct {
		name string
		mode int64
		body string
	}{
		{"llama-" + LlamaCppTag + "/", 0o755 | 0o40000, ""},
		{"llama-" + LlamaCppTag + "/llama-server", 0o755, "#!/bin/sh\necho fake\n"},
		{"llama-" + LlamaCppTag + "/libggml.so", 0o644, "binary-ish"},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body))}
		if e.name[len(e.name)-1] == '/' {
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serveArchive(t *testing.T, archive []byte) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestEnsureAssetDownloadsExtractsFlattens(t *testing.T) {
	archive := fakeArchive(t)
	srv, hits := serveArchive(t, archive)

	dir := t.TempDir()
	p := &Provisioner{Dir: dir, Client: srv.Client(), BaseURL: srv.URL}
	a := asset{name: "llama-" + LlamaCppTag + "-bin-test.tar.gz", sha256: sha256Hex(archive)}

	binPath, err := p.ensureAsset(context.Background(), a)
	if err != nil {
		t.Fatalf("ensureAsset: %v", err)
	}

	// Flattened: llama-server sits directly under the version dir, not nested.
	wantBin := filepath.Join(dir, "llamacpp", LlamaCppTag, "llama-server")
	if binPath != wantBin {
		t.Errorf("binPath = %q, want %q", binPath, wantBin)
	}
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary not executable: %v", info.Mode())
	}
	// Sibling library extracted alongside (rpath resolution depends on it).
	if _, err := os.Stat(filepath.Join(dir, "llamacpp", LlamaCppTag, "libggml.so")); err != nil {
		t.Errorf("sibling lib missing: %v", err)
	}

	// Idempotent: a second call does not re-download.
	if _, err := p.ensureAsset(context.Background(), a); err != nil {
		t.Fatalf("second ensureAsset: %v", err)
	}
	if *hits != 1 {
		t.Errorf("download hits = %d, want 1 (second call should be cached)", *hits)
	}
}

func TestEnsureAssetChecksumMismatch(t *testing.T) {
	archive := fakeArchive(t)
	srv, _ := serveArchive(t, archive)

	dir := t.TempDir()
	p := &Provisioner{Dir: dir, Client: srv.Client(), BaseURL: srv.URL}
	a := asset{name: "x.tar.gz", sha256: "deadbeef"}

	if _, err := p.ensureAsset(context.Background(), a); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	// Nothing installed on failure.
	if _, err := os.Stat(filepath.Join(dir, "llamacpp", LlamaCppTag)); err == nil {
		t.Error("runtime dir should not exist after checksum failure")
	}
}

func TestEnsureLlamaServerUnsupportedPlatform(t *testing.T) {
	p := &Provisioner{Dir: t.TempDir()}
	if _, err := p.EnsureLlamaServer(context.Background(), "plan9", "sparc"); err == nil {
		t.Fatal("expected error for unsupported platform")
	}
}

func TestEnsureLlamaServerKnownPlatforms(t *testing.T) {
	for _, plat := range []string{"darwin/arm64", "darwin/amd64", "linux/arm64", "linux/amd64"} {
		if _, ok := llamaCppAssets[plat]; !ok {
			t.Errorf("missing pinned asset for %s", plat)
		}
	}
}
