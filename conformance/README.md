# Conformance suite

The executable spec of Atlas's compat promise. What it tests and why lives in [docs/conformance-suite.md](../docs/conformance-suite.md); this directory is the harness that runs it.

## Layout

```text
run.py                     # harness runner: starts the target, runs both suites, writes matrix.json
capability_matrix.py       # G18: aggregate per-run matrices into the agent-capability matrix
test_capability_matrix.py  # unit tests for the aggregator (not a conformance group; run.py skips it)
stubgw/                    # deliberately partial stub gateway (phase-1 target; see its docstring)
py/                        # pytest suite: Anthropic Python SDK, OpenAI SDK, wire-level SSE capture
ts/                        # vitest suite: Anthropic TypeScript SDK subset
results/                   # output (gitignored): matrix.json + raw per-suite results
```

## Running

Prerequisites: [uv](https://docs.astral.sh/uv/) and Node 22+.

```sh
make conformance              # from the repo root: npm install + run against the stub, gate on G1
```

Or directly:

```sh
cd conformance
npm install --prefix ts       # once
uv run python run.py                          # against the built-in stub gateway
uv run python run.py --require G1,G2          # exit 1 unless the listed groups are green
uv run python run.py --base-url http://host:port --engine llamacpp --model <id>   # against a real Atlas
```

To run against a real local gateway end to end (what CI does):

```sh
go build -o atlas ./cmd/atlas
# Pull two starter-catalog models from cold: a non-reasoning one and a
# reasoning one (G4 needs both). atlas up would also pull on demand.
./atlas pull qwen2.5-1.5b-instruct qwen3-0.6b
# Auth is per-key since phase 5 (ADR-0008): mint one into the state-dir store the
# gateway reads, then hand it to the harness. (atlas up also prints a default key.)
KEY=$(./atlas keys create --quiet)
./atlas up --model qwen2.5-1.5b-instruct --model qwen3-0.6b \
  --alias claude-sonnet-4-6=qwen2.5-1.5b-instruct \
  --alias claude-haiku-4-5=qwen2.5-1.5b-instruct \
  --alias claude-opus-4-1=qwen3-0.6b \
  > atlas.log 2>&1 &   # provisions llama.cpp, boots from the store, serves :8080
# ATLAS_LOG_FILE lets G10 check that per-request token counts are logged.
cd conformance && ATLAS_LOG_FILE=../atlas.log uv run python run.py \
  --base-url http://127.0.0.1:8080 --api-key "$KEY" \
  --engine llamacpp --model qwen2.5-1.5b-instruct \
  --reasoning-model qwen3-0.6b --require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10
```

G9's `agent-sdk` cell (a streamed ≥3-call tool loop) runs by default. The real Claude Code drop-in cell (`claude-code`) is opt-in — it needs the `claude` binary and a capable enough model (the 1.5B tier drives it only intermittently):

```sh
# CLAUDE_CODE_MAX_OUTPUT_TOKENS is capped to fit Claude Code's ~24k-token
# system prompt inside the model's context window; raise it on a larger model.
CONF_CLAUDE_CODE_SMOKE=1 CLAUDE_CODE_MAX_OUTPUT_TOKENS=2048 \
  ATLAS_LOG_FILE=../atlas.log uv run python run.py \
  --base-url http://127.0.0.1:8080 --api-key "$KEY" \
  --engine llamacpp --model qwen2.5-1.5b-instruct \
  --reasoning-model qwen3-0.6b --require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10
```

The harness is engine-agnostic, so the same suite runs against a vLLM-backed Atlas on a GPU host (the full-matrix tier — not yet wired into CI; see [open-questions.md](../docs/open-questions.md)):

```sh
KEY=$(./atlas keys create --quiet)
./atlas up --engine vllm \
  --model Qwen/Qwen2.5-1.5B-Instruct --model Qwen/Qwen3-0.6B \
  --engine-arg --enable-auto-tool-choice --engine-arg --tool-call-parser --engine-arg hermes \
  --engine-arg --reasoning-parser --engine-arg qwen3 \
  --alias claude-sonnet-4-6=Qwen/Qwen2.5-1.5B-Instruct &   # provisions a uv-managed vLLM venv, serves :8080
CONF_CLAUDE_CODE_SMOKE=1 uv run python run.py --base-url http://127.0.0.1:8080 --api-key "$KEY" \
  --engine vllm --model Qwen/Qwen2.5-1.5B-Instruct --reasoning-model Qwen/Qwen3-0.6B \
  --require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10
```

This vLLM run on a capable GPU model — with `CONF_CLAUDE_CODE_SMOKE=1` enabling the real Claude Code cell — is the full-matrix acceptance tier that **closed out M0** (declared done 2026-06-25; it now runs green on the scheduled nightly — see [m0-acceptance.md](../docs/m0-acceptance.md)).

`--reasoning-model` names the reasoning-capable model the G4 thinking cases target; omit it (e.g. against the stub) and those cases skip.

Exit codes: `0` ran (and any `--require` groups are green), `1` a required group is not green, `2` harness error (suite didn't run, or a group has no tests). Test failures outside `--require` do **not** fail the run — the suite is written before the product, and the matrix records exactly what isn't conformant yet.

## matrix.json

One structured record per test, merged across pytest and vitest — this artifact becomes the published compat matrix:

```json
{
  "schema_version": 1,
  "generated_at": "2026-06-12T21:00:00+00:00",
  "target": { "kind": "stub", "engine": "stub", "model": "stub-small" },
  "summary": {
    "total": 28,
    "passed": 13,
    "failed": 6,
    "skipped": 9,
    "flakes": 0,
    "groups": { "G1": { "pass": 8, "fail": 0, "skip": 0, "status": "pass" } }
  },
  "cells": [
    {
      "id": "py/test_g01_messages_basics.py::test_single_turn_text",
      "suite": "pytest",
      "group": "G1",
      "criterion": 1,
      "client": "anthropic-py",
      "engine": "stub",
      "model": "stub-small",
      "status": "pass",
      "duration_s": 0.05,
      "retries": 0,
      "failure": null,
      "skip_reason": null
    }
  ]
}
```

Group status: `fail` if any cell fails, else `pass` if any passes, else `skip`. A group with no cells at all is a harness error — every group in docs/conformance-suite.md must keep at least one (possibly placeholder) test, so the criterion mapping stays bidirectional. `flakes`/`retries` are reserved: the one-retry flake policy starts mattering when real engines join the matrix.

Every pytest test carries `group` / `criterion` / `client` markers; vitest tests carry the same coordinates in their title as `[Gx][cN][client]`.

## Capability matrix (G18)

`run.py` writes one `matrix.json` per (engine, model) run. `capability_matrix.py` aggregates a fleet's worth of those into the published **agent-capability matrix** — the "works for agents" badge ([roadmap.md](../docs/roadmap.md)), earned by the suite rather than asserted:

```sh
# Aggregate every per-engine run from an acceptance pass into one matrix + table.
uv run python capability_matrix.py results/matrix-vllm.json results/matrix-llamacpp.json \
  --output results/capability-matrix.json --markdown results/CAPABILITY.md
```

Each output row is one (model, engine) with an **agent-readiness verdict** plus the per-group detail:

- `ready` — the agent-critical groups (G3 tool use, G9 the streamed ≥3-call agent loop) pass and no core group failed.
- `partial` — agent-critical pass, but a non-critical compat group failed.
- `incomplete` — an agent-critical group was not exercised (skipped/absent), so the run can't vouch for it.
- `unsupported` — an agent-critical group failed: an SDK agent cannot rely on this model×engine.

`--require-ready` exits non-zero unless every cell is `ready`, for gating a release. The aggregator is stdlib-only (no conformance deps) and unit-tested by `test_capability_matrix.py` — harness tooling, not a conformance group, so `run.py`'s `pytest py/` never collects it; run it with `uv run python -m pytest test_capability_matrix.py`. The per-PR CI job runs the unit tests and generates a single-cell matrix from its llama.cpp run; the **full** model×engine matrix is produced on the nightly capable tier (MLX + CUDA runners — still dormant, see [open-questions.md](../docs/open-questions.md)).

## Status

As of [m0-build-plan](../docs/m0-build-plan.md) phase 11, CI runs the harness against the **real Atlas gateway**, booting two tiny llama.cpp models **from cold via the starter catalog** (`atlas pull qwen2.5-1.5b-instruct qwen3-0.6b` into the content-addressable store, then `atlas up --model qwen2.5-1.5b-instruct --model qwen3-0.6b` plus tier aliases on a CPU runner), gating on the full `--require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10`. Against the real gateway today: G1 (non-streaming `/v1/messages`), G2 (SSE streaming), G3 (tool loop: `tool_use`/`tool_result` round-trip, `input_json_delta` streaming, tool_choice variants, parallel calls, `is_error`), G4 (thinking: `thinking` blocks before text, `thinking_delta` streaming, multi-turn echo, advisory `budget_tokens`, graceful no-op on the non-reasoning model), G5 (`/v1/models` + aliases + context-window metadata), G6 (`count_tokens` parity vs `usage.input_tokens` and alias/canonical agreement), G7's gateway-shape cases (including pre-dispatch context-window rejection), G8 (OpenAI `/v1/chat/completions` with streaming + tools, `finish_reason` mapping, usage fields), G9's `agent-sdk` cell (a streamed ≥3-call client-side tool loop through Atlas), and G10 (ops minimum: `/healthz`, `/readyz` readiness, and per-request token counts in the log read via `ATLAS_LOG_FILE`) pass. The engine-down 529 retry case remains skipped until the harness gains deterministic engine lifecycle control. G9's real-Claude-Code drop-in cell (`claude-code`) is opt-in (`CONF_CLAUDE_CODE_SMOKE`) — the 1.5B model drives it only intermittently; reliable Claude Code and the vLLM full matrix run on the capable/GPU acceptance tier, now wired into the scheduled nightly (`machulav/ec2-github-runner`) and green — the run that closed out M0 (see [m0-acceptance.md](../docs/m0-acceptance.md)). Note: `tool_choice` forcing is best-effort on the pinned llama.cpp build (same doc).

The **stub gateway** (`stubgw/`) — a deliberately partial `/v1/messages` (non-streaming text, Anthropic-shaped errors) — remains in the tree as the default target when no `--base-url` is given. It exists so the harness mechanics can be exercised without standing up a model (the fast local loop); it is no longer the CI gate.
