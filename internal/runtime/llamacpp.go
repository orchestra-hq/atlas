// Package runtime provisions engine runtimes on a worker. M0 ships managed
// runtimes only — pinned prebuilt llama.cpp binaries downloaded into the state
// dir — with a container path arriving at M1 behind the same shape (see
// docs/m0-build-plan.md, "Engine runtime provisioning").
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
	"path/filepath"
	"strings"
)

// DefaultReleaseBaseURL is where pinned llama.cpp release assets are fetched
// from. Overridable (tests, mirrors) via Provisioner.BaseURL.
const DefaultReleaseBaseURL = "https://github.com/ggml-org/llama.cpp/releases/download"

// LlamaCppTag is the pinned llama.cpp release. Upgrades are explicit
// (build-time decision 5): bump the tag and the checksums together, then let
// the conformance matrix gate the change.
const LlamaCppTag = "b9611"

// asset describes one platform's prebuilt archive and its expected digest.
type asset struct {
	name   string
	sha256 string
}

// llamaCppAssets pins the SHA-256 of each supported platform archive for
// LlamaCppTag. Digests are from the GitHub release; verify on upgrade with:
//
//	gh api repos/ggml-org/llama.cpp/releases/tags/<tag> \
//	  --jq '.assets[] | {name, digest}'
var llamaCppAssets = map[string]asset{
	"darwin/arm64": {"llama-" + LlamaCppTag + "-bin-macos-arm64.tar.gz", "544282530e3833b113b739263af822a889c33ded15163d01f7a77bb41622eef8"},
	"darwin/amd64": {"llama-" + LlamaCppTag + "-bin-macos-x64.tar.gz", "87722317da7f170fcd4122d9b6ea94d7b6d27968d29d3cccc490d75c1ad61280"},
	"linux/arm64":  {"llama-" + LlamaCppTag + "-bin-ubuntu-arm64.tar.gz", "ea16ee97d2dd29ddf826e7ff3b04ae9fc3df5bca3db4216cd8610883b356f90d"},
	"linux/amd64":  {"llama-" + LlamaCppTag + "-bin-ubuntu-x64.tar.gz", "8ada3fdae5933813c43551052bdbd2ff5fef10a5ab205827d5d208dbc3725cb7"},
}

// Provisioner downloads and unpacks pinned engine runtimes into a directory,
// idempotently. M0 ships managed runtimes only (m0-build-plan: "Engine
// runtime provisioning"); the container path arrives at M1 behind this same
// shape.
type Provisioner struct {
	// Dir is the runtime root, e.g. <state>/runtimes. Each engine/version
	// unpacks into a subdirectory beneath it.
	Dir string
	// Client fetches release assets; nil uses http.DefaultClient.
	Client *http.Client
	// BaseURL is the release download base; empty uses DefaultReleaseBaseURL.
	BaseURL string
}

// EnsureLlamaServer makes the pinned llama-server available for the given
// platform (GOOS/GOARCH) and returns its absolute path. It is idempotent: if
// the binary is already unpacked, it returns immediately without downloading.
func (p *Provisioner) EnsureLlamaServer(ctx context.Context, goos, goarch string) (string, error) {
	a, ok := llamaCppAssets[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("runtime: no pinned llama.cpp build for %s/%s", goos, goarch)
	}
	return p.ensureAsset(ctx, a)
}

func (p *Provisioner) ensureAsset(ctx context.Context, a asset) (string, error) {
	dest := filepath.Join(p.Dir, "llamacpp", LlamaCppTag)
	binPath := filepath.Join(dest, "llama-server")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	if err := p.downloadAndExtract(ctx, a, dest); err != nil {
		return "", err
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("runtime: llama-server missing after extracting %s", a.name)
	}
	return binPath, nil
}

func (p *Provisioner) downloadAndExtract(ctx context.Context, a asset, dest string) error {
	base := p.BaseURL
	if base == "" {
		base = DefaultReleaseBaseURL
	}
	url := strings.TrimRight(base, "/") + "/" + LlamaCppTag + "/" + a.name

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("runtime: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("runtime: download %s: %w", a.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime: download %s: HTTP %d", a.name, resp.StatusCode)
	}

	// Stream to a temp file while hashing, so we verify before trusting bytes.
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return fmt.Errorf("runtime: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(p.Dir, "llamacpp-*.tar.gz")
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

	// Extract into a staging dir, then atomically swap into place so an
	// interrupted extraction never leaves a half-populated runtime.
	staging, err := os.MkdirTemp(p.Dir, "llamacpp-stage-*")
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

// extractTarGz unpacks a gzipped tar into dest, flattening the archive's
// single top-level directory so binaries and their shared libraries land
// directly in dest (and llama-server finds its dylibs/.so via rpath).
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

// flattenTop strips the archive's leading "llama-<tag>/" directory component.
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
