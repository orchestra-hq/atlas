package runtime

import (
	"context"
	"path/filepath"
)

// SGLangVersion is the pinned sglang package installed into its venv. Upgrades are
// explicit (build-time decision 5): bump the version, then let the conformance
// matrix gate the change. Like vLLM, sglang publishes CUDA wheels resolved by uv
// from PyPI; the pinned version (and the Qwen tool/reasoning parser flags carried
// in the catalog's engine_args) must be re-validated on the first GPU run.
const SGLangVersion = "0.5.10.post1"

// sglangPython is the interpreter uv provisions for the venv. sglang targets recent
// CPython; 3.12 matches the vLLM venv.
const sglangPython = "3.12"

// EnsureSGLang provisions a pinned sglang venv for the given platform and returns
// the absolute path to the venv's python interpreter — sglang is launched as
// `<python> -m sglang.launch_server` (it ships the server as a module), so the
// worker prepends those args. It bootstraps uv, creates a venv pinned to
// sglangPython, and installs sglang[all]==SGLangVersion into it (the [all] extra
// pulls the serving runtime). It is idempotent: a fully provisioned venv is
// returned immediately without invoking uv. The install is heavy and targets CUDA,
// so this path runs on NVIDIA-GPU hosts (like vLLM); the unit tests exercise the
// orchestration with a fake runner rather than installing sglang.
func (p *Provisioner) EnsureSGLang(ctx context.Context, goos, goarch string) (string, error) {
	return p.ensureVenv(ctx, goos, goarch, venvRuntime{
		engine:     "sglang",
		version:    SGLangVersion,
		python:     sglangPython,
		pkg:        "sglang[all]==" + SGLangVersion,
		entrypoint: filepath.Join("venv", "bin", "python"),
	})
}
