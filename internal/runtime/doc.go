// Package runtime provisions engine runtimes on a worker: pinned prebuilt
// binaries for llama.cpp, a managed uv venv for vLLM in M0, with a
// container path arriving at M1 behind the same RuntimeProvisioner
// interface (see docs/m0-build-plan.md, "Engine runtime provisioning").
//
// Populated from phase 2 of docs/m0-build-plan.md.
package runtime
