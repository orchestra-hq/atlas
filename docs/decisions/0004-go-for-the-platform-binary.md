# ADR-0004: Go for the Atlas binary

**Status:** proposed (needs owner sign-off — see open-questions.md)

## Context

The two credible choices, with major precedent each:

- **Go** — Ollama's choice. Single static binary, trivial cross-platform install (`curl | sh`, no Python on the host), excellent concurrency/streaming-proxy ergonomics, strong CLI/daemon tooling.
- **Python** — GPUStack's choice. Same language as the ML ecosystem and the engines themselves, larger contributor pool among ML folks, faster iteration on engine integration glue.

Key observation: since engines run as subprocesses (ADR-0001), the platform binary itself does almost no ML work — it is a gateway, scheduler, supervisor, and proxy. That is Go-shaped work. The DX promise ("one static binary, minutes to first token") is core differentiation (vision.md), and Python distribution (venvs, system Python conflicts) is the single biggest install-pain gap we observe between Ollama and GPUStack.

## Decision

Write the Atlas server/worker/CLI in **Go**. Engine adapters invoke engines as subprocesses or containers; any Python needed lives inside engine-managed environments that the worker provisions, never on the user's PATH.

## Consequences

- Best-in-class install story; static binaries for Linux/macOS (and Windows later).
- Engine environment provisioning (getting vLLM's Python/CUDA stack onto a worker) becomes a explicit worker feature (likely: optional container runtime or managed venv per engine) instead of "it's already Python so pip install" — more upfront work, much better end-user hygiene.
- Some ML-community contributors prefer Python; mitigate by keeping engine adapters thin and the model catalog data-driven (YAML), so most contributions don't require Go.
