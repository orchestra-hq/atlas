package runtime

import (
	"context"
	"fmt"
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
	return p.ensureVenv(ctx, goos, goarch, venvRuntime{
		engine:     "mlx",
		version:    MLXVersion,
		python:     mlxPython,
		pkg:        "mlx-lm==" + MLXVersion,
		entrypoint: filepath.Join("venv", "bin", "python"),
	})
}
