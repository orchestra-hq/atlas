# Conformance suite

The executable spec of Atlas's compat promise. What it tests and why lives in [docs/conformance-suite.md](../docs/conformance-suite.md); this directory is the harness that runs it.

## Layout

```text
run.py        # harness runner: starts the target, runs both suites, writes matrix.json
stubgw/       # deliberately partial stub gateway (phase-1 target; see its docstring)
py/           # pytest suite: Anthropic Python SDK, OpenAI SDK, wire-level SSE capture
ts/           # vitest suite: Anthropic TypeScript SDK subset
results/      # output (gitignored): matrix.json + raw per-suite results
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
curl -sSL -o qwen2.5-1.5b.gguf https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf
curl -sSL -o qwen3-0.6b.gguf https://huggingface.co/Qwen/Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q8_0.gguf
# Two models: a non-reasoning model and a reasoning one (G4 needs both). Repeat --model.
./atlas up --model qwen2.5-1.5b.gguf --model qwen3-0.6b.gguf \
  --alias claude-sonnet-4-6=qwen2.5-1.5b-instruct-q4_k_m \
  --alias claude-haiku-4-5=qwen2.5-1.5b-instruct-q4_k_m \
  --alias claude-opus-4-1=Qwen3-0.6B-Q8_0 \
  --api-key dev-key &   # provisions llama.cpp, serves :8080
cd conformance && uv run python run.py \
  --base-url http://127.0.0.1:8080 --api-key dev-key \
  --engine llamacpp --model qwen2.5-1.5b-instruct-q4_k_m \
  --reasoning-model Qwen3-0.6B-Q8_0 --require G1,G2,G3,G4,G5,G6,G7
```

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

## Status

As of [m0-build-plan](../docs/m0-build-plan.md) phase 6, CI runs the harness against the **real Atlas gateway** (`atlas up` with two tiny llama.cpp models — one non-reasoning, one reasoning — plus tier aliases on a CPU runner), gating on `--require G1,G2,G3,G4,G5,G6,G7` — the phase-6 exit criterion. Against the real gateway today: G1 (non-streaming `/v1/messages`), G2 (SSE streaming), G3 (tool loop: `tool_use`/`tool_result` round-trip, `input_json_delta` streaming, tool_choice variants, parallel calls, `is_error`), G4 (thinking: `thinking` blocks before text, `thinking_delta` streaming, multi-turn echo, advisory `budget_tokens`, graceful no-op on the non-reasoning model), G5 (`/v1/models` + aliases + context-window metadata), G6 (`count_tokens` parity vs `usage.input_tokens` and alias/canonical agreement), and G7's gateway-shape cases (including pre-dispatch context-window rejection) pass. The engine-down 529 retry case remains skipped until the harness gains deterministic engine lifecycle control. G8 fails by design (the OpenAI surface lands in phase 7); the gate widens as phases land. Note: `tool_choice` forcing is best-effort on the pinned llama.cpp build — see [open-questions.md](../docs/open-questions.md).

The **stub gateway** (`stubgw/`) — a deliberately partial `/v1/messages` (non-streaming text, Anthropic-shaped errors) — remains in the tree as the default target when no `--base-url` is given. It exists so the harness mechanics can be exercised without standing up a model (the fast local loop); it is no longer the CI gate.
