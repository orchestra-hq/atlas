package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// UvReleaseBaseURL is where pinned uv release assets are fetched from.
const UvReleaseBaseURL = "https://github.com/astral-sh/uv/releases/download"

// UvVersion is the pinned uv release used to bootstrap the vLLM venv, and
// VLLMVersion is the pinned vLLM package installed into it. Upgrades are
// explicit (build-time decision 5): bump the version(s) and (for uv) the
// checksums together, then let the conformance matrix gate the change.
const (
	UvVersion   = "0.9.2"
	VLLMVersion = "0.23.0"
	// vllmPython is the interpreter uv provisions for the venv. vLLM publishes
	// wheels for 3.10–3.12; 3.12 is the newest it fully supports.
	vllmPython = "3.12"
)

// uvAssets pins the SHA-256 of each supported platform's uv archive for
// UvVersion. Digests are the *.tar.gz.sha256 files from the GitHub release;
// verify on upgrade with, e.g.:
//
//	curl -sSL https://github.com/astral-sh/uv/releases/download/<ver>/<name>.tar.gz.sha256
var uvAssets = map[string]asset{
	"darwin/arm64": {"uv-aarch64-apple-darwin.tar.gz", "90b1e69da3d04772565dd556ae8e72c86bdb7da85a8dfc2c6b50c400b0e6aa97"},
	"darwin/amd64": {"uv-x86_64-apple-darwin.tar.gz", "c887d2c4f629eee99b80a347880870f3bc77f7746db81923efe520f609f80857"},
	"linux/arm64":  {"uv-aarch64-unknown-linux-gnu.tar.gz", "0f0ecf2abcb50f8fb5d2b52c8095af4c133897086e3f2e0259f4fcb3d8ddf273"},
	"linux/amd64":  {"uv-x86_64-unknown-linux-gnu.tar.gz", "b775bb84c72210c6c0b9670cfaad0ac2e3953f12a2947d52b57603b4fbae3798"},
}

// EnsureUv makes the pinned uv binary available for the given platform
// (GOOS/GOARCH) and returns its absolute path. It is idempotent: if uv is
// already unpacked, it returns immediately without downloading.
func (p *Provisioner) EnsureUv(ctx context.Context, goos, goarch string) (string, error) {
	a, ok := uvAssets[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("runtime: no pinned uv build for %s/%s", goos, goarch)
	}
	dest := filepath.Join(p.Dir, "uv", UvVersion)
	binPath := filepath.Join(dest, "uv")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	base := p.BaseURL
	if base == "" {
		base = UvReleaseBaseURL
	}
	if err := p.installRelease(ctx, base, UvVersion, a, dest, "uv-*.tar.gz"); err != nil {
		return "", err
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("runtime: uv missing after extracting %s", a.name)
	}
	return binPath, nil
}

// EnsureVLLM provisions a pinned vLLM venv for the given platform and returns
// the absolute path to the venv's `vllm` entrypoint. It bootstraps uv, creates
// a venv pinned to vllmPython, and installs vllm==VLLMVersion into it. It is
// idempotent: a fully provisioned venv is returned immediately without invoking
// uv. The install is heavy and (for the published wheels) targets CUDA, so this
// path runs on GPU hosts; the unit tests exercise the orchestration with a fake
// runner rather than installing vLLM.
func (p *Provisioner) EnsureVLLM(ctx context.Context, goos, goarch string) (string, error) {
	return p.ensureVenv(ctx, goos, goarch, venvRuntime{
		engine:     "vllm",
		version:    VLLMVersion,
		python:     vllmPython,
		pkg:        "vllm==" + VLLMVersion,
		entrypoint: filepath.Join("venv", "bin", "vllm"),
	})
}
