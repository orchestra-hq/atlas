# M2 acceptance spec

> **✅ Accepted — M2 declared done 2026-06-26.** The two engine-breadth tracks are green on real hardware in one nightly run on `main` ([run 28223896986](https://github.com/orchestra-hq/atlas/actions/runs/28223896986)): **SGLang acceptance (GPU)** on an L4 g6 serving the hybrid `qwen3-8b-sglang`, and **MLX acceptance (Apple Silicon)** on a `blacksmith-6vcpu-macos-latest` runner serving `qwen2.5-7b-instruct-mlx`. Both passed `G1–G8,G10` (incl. G4 thinking on SGLang; G4's thinking cases skip on the non-reasoning MLX tier) and each contributed a **ready** cell to the agent-capability matrix. Combined with the per-PR G15/G16 (observability, backpressure) and G18 (matrix tooling, sampling/reasoning config) gates, all M2 conformance is now proven. The criteria below stay the definition of done.

## What M2 done means

The M2 demo ([roadmap.md](../roadmap.md)): _SSH to the gateway box and `atlas top` to watch the fleet live; push concurrent load past capacity and watch requests queue then shed with clean 429/529 instead of timing out; add an Apple-Silicon worker running MLX._

M2 is **done** when the runtime depth that demo implies is proven at the same bar M0 and M1 cleared:

- the observability surface (`/metrics`, `atlas status`/`top`, stable per-worker usage) reports correct fleet state,
- beyond-capacity load queues then sheds with well-formed retryable `429`/`529` (no hangs, no 5xx),
- and the two new engines — **MLX on Apple Silicon** and **SGLang on NVIDIA** — pass the conformance suite on real hardware, with their results feeding the published agent-capability matrix.

Most of this is **already proven per-PR**; the gap is purely the two engines' real-hardware cells, exactly as M1's gap was purely topological.

## What is already proven per-PR

The gateway-side M2 depth (G15, G16) needs no special hardware and is gated on every PR — it is not what M2-done is waiting on:

- **G15 — observability.** Go unit + integration tests cover `/metrics` series (request/latency/error, per-model and per-worker token counters, in-flight + connected-worker gauges), `atlas status`/`atlas top` against a running gateway, stable worker identity in the ledger across reconnects, and admin-CLI `--tls-pin` against a self-signed gateway (`internal/server/metrics_test.go`, `internal/cli/top_test.go`, `status.go`, `usage_test.go`, `usagewriter.go`).
- **G16 — load balancing + backpressure.** Least-in-flight selection, the bounded per-model admission queue, and `429`/`529` + `Retry-After` shedding are covered by Go tests against a fake-worker stub and a running gateway (`internal/server/admission_test.go`, `gateway_backpressure_test.go`, `selection_test.go`), with the async usage writer proven block-don't-drop under load.
- **G18 — matrix tooling.** `conformance/capability_matrix.py` and its verdict logic are unit-tested (`conformance/test_capability_matrix.py`) and run on the per-PR llama.cpp cell; the sampling-defaults (phase 4a) and reasoning-config (phase 4b) paths are gated on the CPU PR tier.

The single-node and fleet nightlies already feed **vLLM** and **llama.cpp** rows into the capability matrix. The gap is the two engines that need hardware the per-PR runner does not have.

## The gap (decided)

Two new nightly cells, parallel to the existing GPU/CPU/fleet tracks. Both reuse stage 2 (`scripts/acceptance.sh`) unchanged except for per-engine model/arg defaults — the same decoupling the GPU track relies on (ADR-0006):

```text
   ┌──────────────────────────────┐      ┌──────────────────────────────┐
   │ sglang track — NVIDIA GPU    │      │ mlx track — Apple Silicon     │
   │ machulav g6 (same AMI as     │      │ blacksmith-6vcpu-macos-latest │
   │ the vLLM GPU track)          │      │ (Metal; no EC2 provisioning)  │
   │ atlas up --engine sglang     │      │ atlas up --engine mlx         │
   │ G1–G8 + G10                  │      │ G1–G8 + G10                   │
   └──────────────────────────────┘      └──────────────────────────────┘
```

- **SGLang** is a second NVIDIA-GPU server, so its cell runs on the **same on-demand CUDA runner the vLLM GPU track already provisions** (`machulav/ec2-github-runner`, the Deep Learning Base OSS Nvidia Driver AMI) — no new hardware. The pinned `SGLangVersion` (`internal/runtime/sglang.go`) is the one provisioned, and the catalog's SGLang `--tool-call-parser` / `--reasoning-parser` engine_args are observed end-to-end rather than only unit-validated.
- **MLX** runs only on Apple Silicon (`darwin/arm64`, Metal — `EnsureMLX` hard-rejects anything else). Its cell runs on a **`blacksmith-6vcpu-macos-latest`** hosted runner (the project owner's call, 2026-06-25) — a managed macOS runner, so no machulav/EC2 provisioning job: the acceptance step `runs-on` it directly. MLX is provisioned as a managed `uv` venv in the state dir, the same pattern as vLLM/SGLang.

**G9 (the agent harness) is out of the new cells**, matching the build plan's `G17 = G1–G8 + G10`: the capable models on these tracks are 7–8B, which drive a full agent loop only intermittently, so the agent badge stays earned on the larger nightly models, not asserted here. The MLX catalog tier is non-reasoning, so G4's thinking cases skip gracefully (the `reasoning_model` fixture) and its graceful-degradation case runs — MLX proves the surface, not the thinking path. SGLang serves a reasoning model (`qwen3-8b-sglang`), so G4 thinking is covered there.

## Acceptance criteria

All observed on the two new tracks, on real hardware, against a local `atlas up` (single-box; the cross-host transport is M1's proof, already green):

1. **G1–G8 + G10 on SGLang.** The full M0 surface passes served by SGLang on a real GPU — chat, streaming, tools, thinking (`qwen3-8b-sglang` is reasoning-capable), models/aliases, count_tokens, errors, OpenAI surface, context-window assertion.
2. **G1–G8 + G10 on MLX.** The same surface passes served by MLX on Apple Silicon. G4's thinking cases skip (non-reasoning catalog tier); its graceful-degradation case and every other group pass.
3. **Pinned engine version honored.** Each engine runs the version its runtime pins (`SGLangVersion`, `MLXVersion`), provisioned from cold via the managed `uv` venv; the upgrade flow (`atlas runtime upgrade`) moves a runtime to a new pinned version and the suite still passes. (Upgrade-flow mechanics are unit-tested per-PR; the nightly proves cold-boot at the pinned version.)
4. **G18 — capability matrix breadth.** The published matrix (`conformance/capability_matrix.py` aggregating the per-(engine, model) `matrix.json` runs) now carries real **MLX** and **SGLang** rows alongside vLLM and llama.cpp — the agent-readiness verdict per model×engine reflects real runs, not just the CPU cell.
5. **G15/G16 hold under the run.** The observability and backpressure surfaces (proven per-PR) stay green on the nightly tracks — `/metrics` and `atlas status` report the engine's worker correctly; usage is attributed and complete.

## How it is gated

Two tracks added to the nightly acceptance workflow ([nightly-gpu.yml](../../.github/workflows/nightly-gpu.yml)), parallel to the existing single-node GPU/CPU and the M1 fleet tracks:

- a **`sglang`** track — a clone of the vLLM GPU job (machulav GPU runner, same AMI), running `acceptance.sh` with `ACCEPTANCE_ENGINES: sglang`;
- an **`mlx`** track — `runs-on: blacksmith-6vcpu-macos-latest`, no provisioning/teardown jobs, running `acceptance.sh` with `ACCEPTANCE_ENGINES: mlx`.

Both scope the gate to `G1–G8,G10` (via `CONF_REQUIRE`) with the Claude Code smoke off (`CONF_CLAUDE_CODE_SMOKE=0`). A green nightly on both new tracks — feeding MLX + SGLang rows into the capability matrix — is what flips M2 to _done_, the same bar M0 and M1 cleared.

## Build plan (to reach the green run)

1. **`scripts/acceptance.sh`** — add `SGLANG_`/`MLX_` model + engine-arg defaults (capable catalog models per engine) and a `CONF_REQUIRE` env so a track can scope the gate to `G1–G8,G10`. Stage 2 otherwise unchanged — the generic per-engine loop already handles any engine label.
2. **Two tracks in `nightly-gpu.yml`** — the SGLang GPU track (clone the vLLM start/run/stop jobs) and the MLX macOS track (a single job on the Blacksmith hosted runner; install Go/uv/Node via the runner's toolchain). Add both to the default `tracks` string and the gating fallbacks.
3. **Run to green** — dispatch each track on its PR branch and iterate (the M1 pattern: run → fix → push into the same PR), GPU fit and macOS toolchain being the likely first snags.
4. **Record + flip** — on green runs, mark M2 ✅ done in [roadmap.md](../roadmap.md), flip this doc's banner with the run evidence, and resolve the MLX/SGLang/full-matrix items in [open-questions.md](open-questions.md).

## Out of scope for M2-done

- **Web console** and **packaging / reference IaC** — explicitly pulled out of M2 into roadmap [M6](../roadmap.md) and [M5](../roadmap.md) ([m2-build-plan.md](m2-build-plan.md) §5).
- **Richer per-model reasoning _styles_** beyond the `enable_thinking` convention — deferred until such a model enters the shipped catalog ([open-questions.md](open-questions.md)).
- **Session/prefix affinity, HA control plane** — M3, not here ([ADR-0010](decisions/0010-load-balancing-and-backpressure.md)).
- **ACME / public-DNS TLS** — self-signed + pin remains the transport; ACME is an M5 follow-up.
