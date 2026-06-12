# Open questions

Decisions that need the project owner's call (or at least sign-off). When a question is resolved, the outcome is recorded in the doc (or ADR) that owns it and the question is deleted here — git history keeps the trail; this file only ever lists what is actually open.

## How do workers get engine runtimes?

Atlas ships as one static Go binary, but the engines it orchestrates don't: vLLM and SGLang are heavy Python/CUDA stacks; llama.cpp is a native binary with hardware-specific builds; MLX is Python-on-Metal. Something has to put the right engine version on each worker machine. Options:

- **(a) Bring-your-own:** the operator installs engines; Atlas just discovers and shells out. Zero install logic for us, but wrecks the minutes-to-first-token story and invites version-skew bugs we can't reproduce.
- **(b) Containers:** the worker pulls a pinned engine image (Docker/Podman + NVIDIA container toolkit). Reproducible, isolated, easy upgrades/rollback — but requires a container runtime on every box, and is a non-starter for MLX (Metal isn't reachable from Linux containers) and awkward on macOS generally.
- **(c) Managed runtimes:** the worker provisions engines itself — `uv`-managed venvs with pinned versions for Python engines, downloaded prebuilt binaries for llama.cpp. No container dependency, works on bare metal and macOS; the cost is owning installation edge cases (CUDA wheel matrix, GPU driver mismatches) ourselves.

Current recommendation: **hybrid (b)+(c)** — containers where a runtime is present (most CUDA fleets), managed runtimes as the fallback (bare metal, macOS/MLX, air-gapped). The worker probes and picks; the operator can force either.

**Proposal awaiting sign-off:** the [M0 build plan](m0-build-plan.md) sequences this as managed runtimes (c) only in M0 — prebuilt llama.cpp binaries, `uv`-managed venv for vLLM — with the container path (b) landing at M1 behind the same provisioner interface. Signing off on that section closes this question.
