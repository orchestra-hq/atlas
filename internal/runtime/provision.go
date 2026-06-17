// Package runtime provisions engine runtimes on a worker. M0 ships managed
// runtimes only (m0-build-plan: "Engine runtime provisioning"): pinned prebuilt
// llama.cpp binaries and a uv-bootstrapped vLLM venv, both downloaded into the
// state dir — with a container path arriving at M1 behind the same shape.
package runtime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// asset describes one platform's prebuilt archive and its expected digest.
type asset struct {
	name   string
	sha256 string
}

// Provisioner downloads and unpacks pinned engine runtimes into a directory,
// idempotently. M0 ships managed runtimes only; the container path arrives at
// M1 behind this same shape.
type Provisioner struct {
	// Dir is the runtime root, e.g. <state>/runtimes. Each engine/version
	// unpacks into a subdirectory beneath it.
	Dir string
	// Client fetches release assets; nil uses http.DefaultClient.
	Client *http.Client
	// BaseURL overrides the llama.cpp release download base (tests, mirrors);
	// empty uses DefaultReleaseBaseURL.
	BaseURL string
	// run executes a provisioning subprocess (uv) for its side effects. nil uses
	// the real exec runner; tests inject a fake to avoid invoking uv.
	run func(ctx context.Context, name string, args ...string) error
}

func (p *Provisioner) httpClient() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// runCmd runs a provisioning subprocess, routing through the injected runner
// when set. The command's combined output is attached to any error.
func (p *Provisioner) runCmd(ctx context.Context, name string, args ...string) error {
	if p.run != nil {
		return p.run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("runtime: %s %s: %w: %s", name, strings.Join(args, " "), err, truncate(out, 2048))
	}
	return nil
}

// installRelease downloads a GitHub release asset (base/tag/asset.name),
// verifies its sha256, and extracts it into dest atomically: it streams to a
// temp file while hashing (verify before trusting bytes), extracts into a
// staging dir, then swaps it into place so an interrupted run never leaves a
// half-populated runtime. tmpPrefix names the temp file/dir.
func (p *Provisioner) installRelease(ctx context.Context, base, tag string, a asset, dest, tmpPrefix string) error {
	url := strings.TrimRight(base, "/") + "/" + tag + "/" + a.name

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("runtime: build request: %w", err)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("runtime: download %s: %w", a.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime: download %s: HTTP %d", a.name, resp.StatusCode)
	}

	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return fmt.Errorf("runtime: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(p.Dir, tmpPrefix)
	if err != nil {
		return fmt.Errorf("runtime: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime: write archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtime: close archive: %w", err)
	}

	if got := hex.EncodeToString(hasher.Sum(nil)); got != a.sha256 {
		return fmt.Errorf("runtime: checksum mismatch for %s: got %s, want %s", a.name, got, a.sha256)
	}

	staging, err := os.MkdirTemp(p.Dir, tmpPrefix+"stage-")
	if err != nil {
		return fmt.Errorf("runtime: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extractTarGz(tmpName, staging); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("runtime: create dest parent: %w", err)
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("runtime: install runtime: %w", err)
	}
	return nil
}

// extractTarGz unpacks a gzipped tar into dest, flattening the archive's single
// top-level directory so binaries and their shared libraries land directly in
// dest (so a binary finds its dylibs/.so via rpath).
func extractTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("runtime: open archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("runtime: gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("runtime: read tar: %w", err)
		}

		name := flattenTop(hdr.Name)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("runtime: mkdir %s: %w", name, err)
			}
		case tar.TypeReg:
			if err := writeFile(tr, target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := writeSymlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Skip device nodes, fifos, etc. — release archives have none.
		}
	}
	return nil
}

// flattenTop strips the archive's leading top-level directory component.
func flattenTop(name string) string {
	name = filepath.Clean("/" + name)[1:] // normalize, drop leading slash
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return "" // the top-level dir entry itself
}

// safeJoin joins dest and name, refusing paths that escape dest (zip-slip).
func safeJoin(dest, name string) (string, error) {
	target := filepath.Join(dest, name)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("runtime: archive entry %q escapes destination", name)
	}
	return target, nil
}

func writeFile(r io.Reader, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("runtime: mkdir for %s: %w", target, err)
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("runtime: create %s: %w", target, err)
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return fmt.Errorf("runtime: write %s: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("runtime: close %s: %w", target, err)
	}
	return nil
}

func writeSymlink(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("runtime: mkdir for symlink %s: %w", target, err)
	}
	_ = os.Remove(target)
	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("runtime: symlink %s: %w", target, err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
