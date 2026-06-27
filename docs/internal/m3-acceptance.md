# M3 acceptance spec

> **✅ Accepted — M3 declared done 2026-06-26.** The per-PR `Conformance (M3)` job is green on `main` ([run 28233738897](https://github.com/orchestra-hq/atlas/actions/runs/28233738897), PR #91): G19 (affinity hits + warm-key observability across two chat replicas), G20 (embeddings + reranker drop-in + wrong-class 400s), G21 (audit trail with actor/action/target, durable across restart), and G22 (cloud-fallback spill labeled `x-atlas-served-by: cloud` with cloud-class usage; off-path sheds) all pass end-to-end against a real two-process llama.cpp deployment + stub upstream via [`scripts/conformance-m3.sh`](../../scripts/conformance-m3.sh). Combined with the per-PR Go unit/integration gates for the M3 logic, all four phase exit criteria are met. The GPU-engine breadth below remains a tracked, non-blocking nightly-growth follow-on. The criteria below stay the definition of done.

## What M3 done means

The M3 demo ([roadmap.md](../roadmap.md)): _run a multi-turn agent against the fleet and watch each turn stick to its warm replica; deploy an embedding + a reranker model and point a RAG stack's OpenAI client at the same endpoint; flip on cloud-fallback and watch a capacity spike spill to a provider, clearly labeled, instead of shedding._

M3 is **done** when the four new conformance groups pass end-to-end on a real deployment, at the bar M0/M1/M2 cleared:

- **G19** — a conversation sticks to its warm replica while capacity allows; affinity yields to backpressure under load; hit/miss + warm-key visible in `/metrics`.
- **G20** — a deployed embedding model serves `/v1/embeddings` and a reranker serves `/v1/rerank`; a wrong-class request is rejected cleanly.
- **G21** — every control-plane mutation lands in an append-only, restart-durable audit log surfaced by `atlas audit` and `GET /admin/audit`.
- **G22** — with fallback on, overflow spills upstream and is labeled `x-atlas-served-by: cloud` with cloud-class usage; with it off, the same request sheds per ADR-0010.

## What is already proven per-PR

The M3 **logic** is gated on every PR by Go unit + integration tests (`internal/server/affinity_test.go`, `embeddings_test.go`, `cloud_test.go`, `audit_test.go`; `internal/cli/audit_test.go`; `internal/engines/openaichat/embed_test.go`, `rerank_test.go`; `internal/db/audit_test.go`). These cover routing-key derivation and affinity-vs-load tolerance, class routing/rejection, audit record shape + append-only invariant, and fallback enable/label/bill logic against fakes and httptest gateways. The gap M3-done closes is the **end-to-end** proof on a real engine, which the unit tier cannot give.

## The gap (decided)

A new per-PR conformance tier plus nightly GPU breadth, mirroring how G11–G14 (fleet) and G15–G18 (M2) are gated.

### Per-PR — real two-process llama.cpp ([`scripts/conformance-m3.sh`](../../scripts/conformance-m3.sh))

A standalone script (the same shape as the two-process gate steps in [ci.yml](../../.github/workflows/ci.yml)) that brings up `atlas server` + two `atlas worker`s on CPU llama.cpp — worker 1 serving chat + the `nomic-embed-text-v1.5` embedding model + the `bge-reranker-v2-m3` reranker, worker 2 a second chat replica — plus a stub upstream, and asserts:

- **G19** — a burst of identical-prefix requests across two chat replicas accrues affinity **hits**, an `x-atlas-session` pin routes by that key, and `atlas_affinity_total{result="hit"}` + `atlas_affinity_warm_keys` appear in `/metrics`. (The load-yield-to-backpressure path is covered by `affinity_test.go`; the per-PR scenario proves the affine selection and its observability on a real deployment.)
- **G20** — `/v1/embeddings` returns correct-shape vectors for a multi-input request; `/v1/rerank` orders documents by relevance (the on-topic document ranks first); an embeddings call against a chat model and a rerank call against an embedding model each return **400**.
- **G21** — `atlas deploy`/`atlas stop` (HTTP admin) and `atlas keys create`/`revoke` (local CLI) produce `deployment.set` / `deployment.stop` / `key.create` / `key.revoke` audit records with the right actor/target; `GET /admin/audit` returns the same trail; the trail survives a server restart (append-only + durable).
- **G22** — a model served only upstream 404s locally and **spills** to the stub, returning `200` with `x-atlas-served-by: cloud` and the upstream body, with usage attributed to a `cloud:<provider>` ledger class; a model with no fallback configured sheds locally and is never labeled cloud-served.

All four are CPU-only (no GPU, no special hardware) and run on every PR.

### Nightly — GPU-engine breadth (standing growth, not a gate)

A breadth dimension layered on top of the per-PR proof, analogous to how M2's **full** capability matrix is a standing pin-on-verify activity rather than a done-gate ([open-questions.md](open-questions.md)): the genuine prefix-cache **reuse** that affinity exists to win is best shown on a prefix-caching GPU engine (SGLang), and the embedding/rerank classes wrap the GPU engines' embedding + scoring tasks ([ADR-0012](decisions/0012-embeddings-and-reranker-model-classes.md)), so observing them on a GPU engine validates that wrapping beyond llama.cpp. This needs GPU embedding/reranker catalog rows (pinned digests) that the starter catalog does not yet carry, so it grows on the nightly tier as those rows are added — it does **not** gate M3-done, because the four phase exit criteria (G19–G22) are each met end-to-end on the real per-PR deployment. Tracked in [open-questions.md](open-questions.md).

## Acceptance criteria

1. **G19–G22 green per-PR** on the real two-process llama.cpp deployment via `scripts/conformance-m3.sh`, wired as the `Conformance (M3)` CI job — the M3-done gate. This proves each phase's exit criterion end to end on a real engine: affine selection + hit/warm-key observability (G19), embeddings + rerank drop-in + wrong-class rejection (G20), the durable append-only audit trail (G21), and labeled cloud-fallback spill + off-path shedding (G22).
2. **All prior groups stay green** (G1–G18), per the cumulative exit-criteria rule.

GPU-engine breadth (above) is a documented standing follow-on, not part of the gate.

## How it is gated

- A per-PR **`Conformance (M3)`** job in [ci.yml](../../.github/workflows/ci.yml) runs `scripts/conformance-m3.sh` on the CPU runner. A green run is what flips M3 to _done_ — the same per-PR-conformance bar M0–M2 cleared, with the GPU breadth growing on the nightly tier afterward.

## Build plan (to reach the green run)

1. **`scripts/conformance-m3.sh`** — the per-PR G19–G22 scenarios on real llama.cpp + a stub upstream (validated locally on Apple Silicon first).
2. **Wire it into `ci.yml`** as the `Conformance (M3)` job.
3. **Run to green** and iterate (the established run → fix → push loop).
4. **Record + flip** — mark M3 ✅ done in [roadmap.md](../roadmap.md), flip this banner with the run evidence, and update the status lines in `README.md` / `CLAUDE.md`.
5. **Grow the GPU breadth** on the nightly tier afterward, as GPU embedding/reranker catalog rows are pinned (tracked, non-blocking).

## Out of scope for M3-done

- **Full HA control plane** (Postgres, durable admission queue, multi-replica) — explicitly deferred to its own future milestone ([m3-build-plan.md](m3-build-plan.md) §5; ADR-0008/ADR-0010 left the doors open).
- **Hosted control-plane offering** — a business decision, not a build task.
- **Audit tamper-evidence (hash-chaining)** — recorded as a future option in the ADR-0008 lineage, not built here.
- **Affinity load-yield determinism on CPU** — the yield-to-backpressure path stays unit-tested; the per-PR conformance proves affine selection + observability, and the nightly proves genuine prefix-cache reuse on SGLang.
