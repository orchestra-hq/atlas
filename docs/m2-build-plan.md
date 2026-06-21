# M2 build plan

The ordered path from M1's multi-machine fleet to a fleet you can **operate from the terminal**: see what it is doing, keep it healthy under load, run it on more hardware, and trust the catalog. This refines the M2 milestone in [roadmap.md](roadmap.md); design truth still lives in [architecture.md](architecture.md) and the relevant ADRs.

M2 is deliberately terminal-first. The **web console** and **packaging + reference IaC** that earlier drafts put in M2 are pulled out into their own later milestones (roadmap M6 and M5) — the console because operating the fleet from the CLI defers the need for a GUI, and packaging because it is a large, independent body of ops work. What is left is the runtime depth and observability that make a fleet operable at all.

## Build-time technical decisions

Choices recorded here so they don't get re-litigated mid-build:

1. **Observability is Prometheus + structured logs; conventions live in this doc, not an ADR.** Metrics use `prometheus/client_golang` exposed at `/metrics` on the existing server listener (admin-scoped); logs stay `slog` with consistent fields. Metric names and label sets are a convention, recorded here, not a load-bearing architectural decision.
2. **The CLI inspection tool reuses the metrics/registry data, no second data path.** `atlas status` (one-shot snapshot) and `atlas top` (auto-refreshing terminal view) read the same counters `/metrics` exposes, served over the admin API. The operator SSHes to the gateway box (or points `--server` + an admin key at it) to watch the fleet. This is the stand-in for the deferred web console.
3. **Load balancing and backpressure follow [ADR-0010](decisions/0010-load-balancing-and-backpressure.md):** least-in-flight replica selection, a bounded per-model admission queue, and retryable `429`/`529` (Anthropic envelope, mirrored on OpenAI) with `Retry-After`. It is gateway-side; no new wire protocol; the queue is in-memory (HA is M3). Session/prefix affinity is M3, not here.
4. **New engines follow [ADR-0001](decisions/0001-orchestrate-engines-not-build-one.md) and the established managed-runtime pattern — no new ADR.** MLX and SGLang are OpenAI-compatible servers, so both reuse `internal/engines/openaichat` exactly as the vLLM adapter does, and both are provisioned as managed runtimes in the state dir (a `uv`-managed Python venv, like vLLM). Engine versions are pinned (as `VLLMVersion` already is) with an explicit upgrade flow. This is more of the same, documented here rather than in an ADR.
5. **Web console and packaging/IaC are out of M2.** They are roadmap M6 (web console) and M5 (packaging & deployment), respectively. M2 must not grow a GUI or a deploy-recipe surface.

## Phases

Exit criteria are cumulative: each phase must hold all prior groups green.

| Phase | Deliverable                                                                                                                                                                    | Exit criterion                                                                                                                                                                                                                            |
| ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1     | Observability: Prometheus `/metrics` + `atlas status`/`atlas top` CLI; stable worker identity in the ledger; admin-CLI cert pinning                                            | G15 (`/metrics` scrape and `atlas status` report correct fleet state across N requests and a worker drop; `atlas usage --by-worker` shows stable names; the CLI reaches a self-signed-TLS gateway via `--tls-pin`)                        |
| 2     | Load balancing + backpressure ([ADR-0010](decisions/0010-load-balancing-and-backpressure.md)): least-in-flight selection, bounded per-model queue, `429`/`529` + `Retry-After` | G16 (beyond-capacity concurrent load queues then sheds with well-formed `429`/`529` — no hangs, no 5xx; load spreads across replicas; queue depth and shed counts visible in `/metrics` and `atlas top`; usage stays complete under load) |
| 3     | Engine breadth: MLX adapter, then SGLang adapter; engine version pinning + upgrade flow                                                                                        | G17 (G1–G8+G10 green on MLX and SGLang on the capable/nightly tier; the pinned engine version is honored and the upgrade flow swaps it cleanly)                                                                                           |
| 4     | Catalog expansion + published agent-capability matrix; apply catalog sampling defaults + per-model reasoning config                                                            | G18 (capability matrix generated from real per-model×engine runs; a request omitting `temperature` uses the catalog default; a reasoning model toggles per its catalog config) — **M2 done**                                              |

## Phase notes

**Phase 1 — observability and the CLI inspection tool.** This builds the measurement substrate everything downstream surfaces. A `/metrics` endpoint (`prometheus/client_golang`) exposes request rate, latency, and error histograms; per-model and per-worker token counters; in-flight gauges; and queue-depth/shed counters (wired as zero placeholders here, populated in phase 2). `atlas status` prints a one-shot fleet snapshot; `atlas top` is an auto-refreshing terminal view (workers, loaded models, in-flight counts, tokens/sec, recent errors) — together they are how an operator sees "what is happening right now" without a console. The phase folds in two rehomed review follow-ups that this surface depends on: the usage ledger's **stable worker identity** (record the worker's `--name`, not the ephemeral per-connection id, so `atlas usage --by-worker` and the per-worker views don't fragment across reconnects — see [follow-ups.md](follow-ups.md)), and **admin-CLI cert pinning** (a shared `--tls-pin`/`ATLAS_TLS_PIN` on the admin clients, so the CLI inspection tool can reach a self-signed-TLS gateway, which it could not in M1).

Phase 1 lands in two PRs: **1a** — `/metrics` + `atlas status` + the stable-worker-identity and admin-pin follow-ups; **1b** — the live `atlas top` view + structured-log polish.

**Phase 2 — load balancing and backpressure.** The serving-path depth, per [ADR-0010](decisions/0010-load-balancing-and-backpressure.md). Replica selection moves from round-robin to **least-in-flight** (each route carries an in-flight counter incremented at dispatch, decremented on every completion path). A **bounded per-model admission queue** caps concurrency at the sum of a model's replica slots; beyond it, requests wait briefly then shed with a retryable **429** (capacity exists, momentarily full) or **529** (no capacity / overloaded), both in the Anthropic envelope and mirrored on OpenAI, both carrying `Retry-After`. It is entirely gateway-side — no new wire messages. The phase folds in three rehomed follow-ups that live on this same path: the **single resolve-with-intent entry point** (collapse the `resolveOrStart`-vs-`resolve` copy-paste hazard into one entry that takes an explicit intent — done while the selection code is open anyway), the **auto-start readiness signal** (replace `EnsureModel`'s 50 ms busy-poll with an event fired from `ModelReady`/`LoadFailed`, which matters more once requests queue under load), and the **async batched usage writer** (move the per-request SQLite `INSERT` off the hot path onto a background batch writer, so the ledger does not become a writer bottleneck under the very load this phase introduces).

Phase 2 lands in two PRs: **2a** — least-in-flight selection + the resolve-with-intent refactor + the readiness signal; **2b** — the admission queue, backpressure, `429`/`529` semantics, and the async usage writer. G16 is verified through phase 1's surface: the queue-depth and shed counters light up in `/metrics` and `atlas top` under a beyond-capacity load test.

**Phase 3 — engine breadth and version pinning.** Atlas wraps engines, it does not build them ([ADR-0001](decisions/0001-orchestrate-engines-not-build-one.md)); M0/M1 shipped llama.cpp (everywhere) and vLLM (NVIDIA). M2 adds two more, both OpenAI-compatible so both reuse `internal/engines/openaichat`:

- **3a — MLX** (`mlx_lm.server`) makes Apple-Silicon Mac workers first-class. This is the higher-value addition: the M1 fleet demo already includes a Mac, but a Mac worker today can only run llama.cpp. MLX is provisioned as a managed `uv` venv in the state dir, the same pattern as vLLM.
- **3b — SGLang** is a second NVIDIA-GPU server alongside vLLM, with prefix-caching and structured-output strengths. Same adapter reuse, same managed-venv provisioning.
- **3c — version pinning + upgrade flow** generalizes the already-pinned vLLM version: each engine has a pinned runtime version (recorded where the runtime is provisioned), and an explicit upgrade path re-provisions to a new pinned version without leaving a half-upgraded runtime.

G17 runs the G1–G8+G10 suite against MLX (Apple-Silicon runner) and SGLang (GPU runner) on the capable/nightly tier — neither runs on the CPU PR runner, so the per-PR gate stays llama.cpp and these are asserted nightly, the same arrangement vLLM has.

**Phase 4 — catalog expansion and the agent-capability matrix.** The catalog grows beyond the starter set (more tiers, pinned gguf/weights digests as each is verified — the M0 pinning rule still holds), and the G9-style agent suite is run per model×engine to produce a published **"works for agents" capability matrix** — the badge earned by the suite, not by vibes (a standing-track promise in [roadmap.md](roadmap.md)). This phase also closes two M0 open-questions whose data the gateway already records but does not yet apply: **per-model sampling defaults** (a request that omits `temperature`/`top_p` picks up the catalog entry's defaults) and **per-model reasoning config** (the catalog's reasoning flag/parser drives thinking control instead of the global `enable_thinking` convention) — both move from "recorded" to "applied", and the corresponding entries in [open-questions.md](open-questions.md) are resolved. G18 asserts the matrix is generated from real runs and that both config paths take effect.

## Conformance groups added in M2

These extend the suite defined in [conformance-suite.md](conformance-suite.md).

### G15 — Observability

`/metrics` exposes the core series (request rate/latency/error, per-model and per-worker token counters, in-flight gauges) and they move correctly across N requests and a worker drop. `atlas status` reports the same fleet state; `atlas usage --by-worker` attributes usage to stable worker names that survive a reconnect (not ephemeral per-connection ids). The admin CLI reaches a self-signed-TLS gateway with `--tls-pin`.

### G16 — Load balancing + backpressure

Under concurrent load beyond the fleet's capacity for a model: requests spread across replicas by least-in-flight, excess requests queue up to the bound, and further requests shed with a well-formed retryable `429` (capacity momentarily full) or `529` (overloaded) carrying `Retry-After` — never a hang or a 5xx. Queue depth and shed counts appear in `/metrics`/`atlas top`, and usage records stay complete and correct under the load.

### G17 — Engine breadth

G1–G8+G10 pass on MLX (Apple Silicon) and on SGLang (NVIDIA GPU), on the capable/nightly tier. The pinned engine version is the one provisioned; the upgrade flow moves a runtime to a new pinned version and the suite still passes.

### G18 — Catalog + capability matrix

The agent-capability matrix is generated from real per-model×engine runs and reflects suite results. A request that omits sampling fields uses the model's catalog defaults; a reasoning model's thinking behavior follows its catalog config rather than the global convention.

## Testing tiers

M2 reuses M1's tier structure, with the capable/nightly tier doing more work:

| Tier        | What                                                                                                                       | When              |
| ----------- | -------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| Unit        | Metric registration, least-in-flight selection, queue admission/shed logic, async-usage batching                           | Every PR          |
| Integration | Gateway admission + backpressure against a fake worker stub; `/metrics` and `atlas status`/`top` against a running gateway | Every PR          |
| Conformance | Real llama.cpp, two-process local deployment (G15, G16)                                                                    | Every PR          |
| Full matrix | MLX (Apple-Silicon runner) and SGLang (GPU runner) for G17; agent-capability matrix for G18                                | Nightly + release |

The per-PR conformance tier proves observability and backpressure on llama.cpp (CPU, no special hardware). MLX needs an Apple-Silicon runner and SGLang a GPU runner, so G17/G18 join the existing nightly capable tier alongside the vLLM run.
