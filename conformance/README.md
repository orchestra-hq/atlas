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

## Phase 1 status

The harness currently targets the **stub gateway** (`stubgw/`) — a deliberately partial `/v1/messages` (non-streaming text, Anthropic-shaped errors) that exists so the harness could be proven before any Atlas code. Against it: G1 and G7's envelope subset pass; G2/G3/G8 fail by design (streaming, tools, and the OpenAI surface don't exist yet); the rest are skipped placeholders citing their build phase. CI gates on `--require G1`; the gate widens as [m0-build-plan](../docs/m0-build-plan.md) phases land, and the target flips from stub to real gateway in phase 2.
