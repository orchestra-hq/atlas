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

// venvRuntime describes a uv-managed engine venv to provision: the engine's
// subdirectory under the runtime root, its pinned version, the interpreter uv
// creates the venv with, the pip spec to install, and the entrypoint path within
// the version directory whose presence means "fully provisioned".
type venvRuntime struct {
	engine     string
	version    string
	python     string
	pkg        string
	entrypoint string // relative to <Dir>/<engine>/<version>, e.g. venv/bin/vllm
}

// ensureVenv provisions a uv-managed engine venv atomically and returns its
// entrypoint path. If the version is already fully provisioned it returns
// immediately. Otherwise it creates a **relocatable** venv in a staging directory,
// installs the pinned package, and swaps the staging directory into
// <Dir>/<engine>/<version> with a single os.Rename — so an interrupted install
// (a crash mid-`pip install`) never leaves a partial venv that the entrypoint
// check would wrongly trust. The venv is created with `--relocatable` so console
// scripts keep working after the swap moves the tree (M2 phase 3c). Stale staging
// directories from a previously killed run are swept first.
func (p *Provisioner) ensureVenv(ctx context.Context, goos, goarch string, r venvRuntime) (string, error) {
	dest := filepath.Join(p.Dir, r.engine, r.version)
	binPath := filepath.Join(dest, r.entrypoint)
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	uv, err := p.EnsureUv(ctx, goos, goarch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return "", fmt.Errorf("runtime: create dir: %w", err)
	}
	p.sweepStaging(r.engine)

	staging, err := os.MkdirTemp(p.Dir, r.engine+stagePrefix)
	if err != nil {
		return "", fmt.Errorf("runtime: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // no-op after a successful rename

	venv := filepath.Join(staging, "venv")
	if err := p.runCmd(ctx, uv, "venv", venv, "--python", r.python, "--relocatable"); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, uv, "pip", "install", "--python", venv, r.pkg); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(staging, r.entrypoint)); err != nil {
		return "", fmt.Errorf("runtime: %s entrypoint missing after install (expected %s)", r.engine, r.entrypoint)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("runtime: create dest parent: %w", err)
	}
	_ = os.RemoveAll(dest) // replace any partial leftover at the final path
	if err := os.Rename(staging, dest); err != nil {
		return "", fmt.Errorf("runtime: install %s runtime: %w", r.engine, err)
	}
	return binPath, nil
}

// stagePrefix tags a venv staging directory so a sweep can recognize leftovers
// from a killed run. The trailing dash separates it from the random suffix.
const stagePrefix = "-stage-"

// sweepStaging removes any leftover staging directories for an engine — debris a
// hard-killed provision (no defer ran) would otherwise accumulate. Best-effort.
func (p *Provisioner) sweepStaging(engine string) {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), engine+stagePrefix) {
			_ = os.RemoveAll(filepath.Join(p.Dir, e.Name()))
		}
	}
}

// ProvisionedVersions lists the version subdirectories present for an engine under
// the runtime root (the directories Ensure* create), newest-first-undefined order.
// Returns nil with no error when the engine has nothing provisioned. Staging
// leftovers are excluded.
func (p *Provisioner) ProvisionedVersions(engine string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.Dir, engine))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: list %s versions: %w", engine, err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	return versions, nil
}

// Prune removes every provisioned version of an engine except keep, returning the
// versions removed. It is how `atlas runtime upgrade --prune` reclaims disk after a
// version bump leaves the superseded runtime behind. A missing engine dir is a
// no-op.
func (p *Provisioner) Prune(engine, keep string) ([]string, error) {
	versions, err := p.ProvisionedVersions(engine)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, v := range versions {
		if v == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.Dir, engine, v)); err != nil {
			return removed, fmt.Errorf("runtime: prune %s %s: %w", engine, v, err)
		}
		removed = append(removed, v)
	}
	return removed, nil
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
