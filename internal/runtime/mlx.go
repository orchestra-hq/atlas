package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// MLXVersion is the pinned mlx-lm package installed into the MLX venv. Upgrades
// are explicit (build-time decision 5): bump the version, then let the conformance
// matrix gate the change. MLX is Apple-Silicon only — its server runs on Metal, so
// there is no checksum-pinned prebuilt binary the way llama.cpp has; the wheel is
// resolved by uv from PyPI like vLLM's.
const MLXVersion = "0.31.3"

// mlxPython is the interpreter uv provisions for the venv. mlx-lm targets recent
// CPython; 3.12 matches the vLLM venv.
const mlxPython = "3.12"

// EnsureMLX provisions a pinned mlx-lm venv for the given platform and returns the
// absolute path to the venv's python interpreter — MLX is launched as
// `<python> -m mlx_lm.server` (mlx-lm exposes the server as a module, not a stable
// console script), so the worker prepends those args. It bootstraps uv, creates a
// venv pinned to mlxPython, and installs mlx-lm==MLXVersion into it. It is
// idempotent: a fully provisioned venv is returned immediately without invoking uv.
//
// MLX only runs on Apple Silicon (it executes on the Mac's unified-memory GPU via
// Metal), so this refuses any platform other than darwin/arm64 rather than
// provisioning a venv that could never serve.
func (p *Provisioner) EnsureMLX(ctx context.Context, goos, goarch string) (string, error) {
	if goos != "darwin" || goarch != "arm64" {
		return "", fmt.Errorf("runtime: MLX requires Apple Silicon (darwin/arm64); this host is %s/%s", goos, goarch)
	}
	dest := filepath.Join(p.Dir, "mlx", MLXVersion)
	venv := filepath.Join(dest, "venv")
	binPath := filepath.Join(venv, "bin", "python")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	uv, err := p.EnsureUv(ctx, goos, goarch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("runtime: create mlx dir: %w", err)
	}

	// Create the venv, then install pinned mlx-lm into it. uv resolves and caches
	// wheels in the state dir; no host Python is touched.
	if err := p.runCmd(ctx, uv, "venv", venv, "--python", mlxPython); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, uv, "pip", "install", "--python", venv, "mlx-lm=="+MLXVersion); err != nil {
		return "", err
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("runtime: mlx venv python missing after install (expected %s)", binPath)
	}
	return binPath, nil
}
