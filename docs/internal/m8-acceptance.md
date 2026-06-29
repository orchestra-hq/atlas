# M8 acceptance spec

> **🚦 Gate green; milestone declaration pending the owner.** The per-PR `Conformance (M8)` job is green on `main`: **G23** proves `atlas up --model <hugging-face-repo>` for a known-family repo with **no catalog row** auto-configures from the model's own metadata and serves an **agent-grade** endpoint, end to end on a real llama.cpp deployment, via [`scripts/conformance-m8.sh`](../../scripts/conformance-m8.sh). Introduced in [#136](https://github.com/orchestra-hq/atlas/pull/136) and stabilized in [#138](https://github.com/orchestra-hq/atlas/pull/138) (skip the redundant, flake-prone anthropic-ts subset — the required groups are fully covered by the pytest Python-SDK + agent-SDK suites). Combined with the per-PR Go unit/integration gates for the M8 resolution logic, all six M8 phase exit criteria are met. The GPU-engine breadth below remains a tracked, non-blocking nightly-growth follow-on. The criteria below stay the definition of done; flipping M8 to **done** in the roadmap is the owner's call.

## What M8 done means

The M8 pitch ([roadmap.md](../roadmap.md)): **`atlas up --model <hugging-face-repo>` for _any_ model.** Atlas fetches the model's metadata (not its weights), decides whether it can serve the model well, and either auto-configures and serves it or tells the user exactly how to add support — turning the curated catalog from a fence into a fast path, with **no catalog for users to maintain and no community catalog for us to maintain** ([ADR-0015](decisions/0015-bring-any-model-auto-configuration.md)).

M8 is **done** when that promise is proven end-to-end on a real deployment, at the bar M0–M3 cleared:

- A newcomer runs `atlas up --model <a-popular-HF-repo-not-in-the-catalog>` and, for a **known family**, is serving an **agent-grade** endpoint (tool calls + reasoning correct) in one command — with no catalog entry written by anyone.
- An **unsupported architecture** or an **oversized** model fails fast, **before** downloading weights, with an actionable message (upstream-engine pointer / memory shortfall) instead of a confusing half-broken serve.
- An engine-**loadable but unknown-family** model serves chat with a clear startup warning + the exact one-line "add `<family>` here" PR pointer, refusable with `--require-verified`.

## What is already proven per-PR (the Go tier)

The M8 **logic** is gated on every PR by Go unit + integration tests across the phases:

- **Metadata → `Capabilities` inspection** (P8.1): the HF `resolve`-URL fetcher, the GGUF header reader, token/revision/gated-repo handling, and the capability resolver (`internal/modelmeta`), exercised against httptest HF endpoints and recorded fixtures.
- **Family map + auto-config resolution** (P8.2): `modelmeta.Classify`, the family→parser/reasoning map, and `resolveRaw` producing a full plan for a known family (`internal/cli/resolve_test.go`, `internal/modelmeta/family_test.go`).
- **Fit/load gate** (P8.3): the per-engine-version arch-loadability check + the VRAM/RAM fit pre-check and their failure messages (`internal/modelmeta/arch_test.go`, the fit estimate, `gateLoadFit`).
- **Warn-and-serve middle case + `--require-verified`** (P8.4): the unknown-family warning, the refuse path, and the `inspect` funnel pointer.

The gap M8-done closes is the **end-to-end** proof on a real engine — that an auto-configured plan, derived from a real repo's metadata with no catalog row, actually serves agent-grade — which the unit tier cannot give.

## The gap (decided)

A new per-PR conformance group plus nightly GPU breadth, mirroring how G15–G18 (M2) and G19–G22 (M3) are gated.

### Per-PR — real single-node llama.cpp ([`scripts/conformance-m8.sh`](../../scripts/conformance-m8.sh))

A standalone script (the same shape as `scripts/conformance-m3.sh`) that builds `atlas`, mints a key, and runs `atlas up --model <known-family-GGUF-repo>` for a repo that is **not** a catalog name — so it flows through `resolveRaw` → `modelmeta.Classify`, the exact auto-config path — then asserts and drives:

- **G23** — `atlas up` prints the `Auto-configured … family` resolution signal (it took the `Classify` path, **not** the bare passthrough) and **no** unknown-family plain-chat warning (it is a _known_ family); the auto-configured endpoint then passes the **agent-critical** conformance gates **G3** (tool loop) and **G9** (the streamed ≥3-call agent-SDK loop), plus **G1/G2** substrate and **G4** reasoning, all via the Anthropic **Python SDK + agent-SDK** suites (`run.py --skip-ts`). The gate model is **`Qwen/Qwen3-0.6B-GGUF`** — a Qwen-published official GGUF (GGUF `general.architecture = qwen3` → the known `qwen3` family, reasoning-capable, ~0.7 GiB), served via llama.cpp's `-hf` with its download cached (keyed on the repo id).

The auto-config assertion is load-bearing: it fails if the `Auto-configured …` line is absent (a silent regression to the bare passthrough) or if the unknown-family warning appears, so the gate distinguishes "auto-configured & agent-grade" from "merely served."

### Nightly — GPU-engine breadth (standing growth, not a gate)

A breadth dimension layered on the per-PR proof, analogous to M2's full capability matrix and M3's prefix-cache-reuse tiering: the **parser-flag** dimension of auto-config (`--tool-call-parser hermes`/`qwen25`/… and the reasoning parsers families set on **vLLM/SGLang**) is invisible on llama.cpp, which drives tool-calling from the chat template, so the per-PR CPU tier proves classification + reasoning gating + sampling + context but not the parser flags. Observing auto-config on a parser-flag engine (vLLM/SGLang) and on **MLX** (Apple Silicon) validates the family map's per-engine args beyond llama.cpp. This grows on the nightly tier and does **not** gate M8-done, because the phase exit criteria are each met end-to-end on the real per-PR deployment. Tracked in [open-questions.md](open-questions.md).

## Acceptance criteria

1. **G23 green per-PR** on the real single-node llama.cpp deployment via `scripts/conformance-m8.sh`, wired as the `Conformance (M8)` CI job — the M8-done gate. It proves the milestone promise end to end: a catalog-less known-family repo auto-configures (the `Auto-configured … family` signal) and serves agent-grade (G3 tool use + G9 agent loop, plus G1/G2/G4).
2. **All prior groups stay green** (G1–G22), per the cumulative exit-criteria rule.

GPU-engine breadth (above) is a documented standing follow-on, not part of the gate.

## How it is gated

- A per-PR **`Conformance (M8)`** job in [ci.yml](../../.github/workflows/ci.yml) runs `scripts/conformance-m8.sh` on the CPU runner. A green run is what flips M8 to _done_ — the same per-PR-conformance bar M0–M3 cleared, with the GPU breadth growing on the nightly tier afterward.

## Build plan (to reach the green run)

1. **`scripts/conformance-m8.sh`** — the per-PR G23 scenario on a real auto-configured llama.cpp serve (validated locally on Apple Silicon first). (P8.5a)
2. **Wire it into `ci.yml`** as the `Conformance (M8)` job, caching the engine's `-hf` download. (P8.5a)
3. **Run to green** and iterate (the established run → fix → push loop — the cold-runner anthropic-ts flake was the one iteration, fixed in #138). (P8.5a)
4. **Reconcile public prose** — the models guide + launch post promise auto-config + the honest verdict, not "best-effort." (P8.5b)
5. **Record + flip** — this report + the doc reconciliation; flipping M8 ✅ done in [roadmap.md](../roadmap.md) and the `README.md` / `CLAUDE.md` status lines is the owner's call. (P8.5c)
6. **Grow the GPU breadth** on the nightly tier afterward (parser-flag families on vLLM/SGLang, auto-config on MLX) — tracked, non-blocking.

## Out of scope for M8-done

- **Models needing `trust_remote_code` / custom architectures** — security + unsupported, excluded by ADR-0015 §6.
- **Auto-config of a local safetensors directory** — stays bare passthrough / `--require-verified`-refusable (carried from P8.2–P8.4); not part of the G23 gate.
- **Real Claude Code drop-in on an auto-configured model** — the capable/GPU acceptance tier (as for M0), not the per-PR CPU gate; G9's agent-SDK loop is the per-PR agent proof.
- **A `--require-verified` global default** — the flag stays per-invocation (P8.4 deferral).
