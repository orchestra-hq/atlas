# M1 acceptance spec

> **Status: in progress.** M1's build is code-complete (phases 1–7, G11–G14 green per-PR on a same-machine two-process deployment — see [m1-build-plan.md](m1-build-plan.md)). What remains to declare M1 _done_ is the **full-matrix tier**: G11–G14 observed on a genuine **multi-machine** deployment, which the per-PR loopback fleet job does not prove. This doc is the definition of done for that run, mirroring [m0-acceptance.md](m0-acceptance.md).

## What M1 done means

The M1 demo ([roadmap.md](roadmap.md)): _`atlas server` on one host; `atlas worker --join` on separate hosts running different engines; deploy two models; one authenticated endpoint serves both._ M1 is **done** when that demo is observed green on real, separate machines — not just the correctness logic on one CI runner over loopback.

The fleet **logic** is already well-tested per-PR (the `conformance-fleet` CI job runs a real `atlas server` + `atlas worker --join` and exercises G1–G8, G10, G11, G12, G13, and G14's drain/timeout/wss cases). The gap is purely **topological**: everything per-PR runs on one runner over `ws(s)://127.0.0.1`, on the CPU/llama.cpp engine only. M1-done observes the same groups across genuinely separate hosts, with the GPU/vLLM engine in the fleet.

## Topology (decided)

A **heterogeneous two-engine fleet** (the project owner's call; the Apple-Silicon/MLX leg is deferred — it depends on an Apple-Silicon runner that does not exist yet, also an open M2 item):

```text
            ┌─────────────────────────────┐
            │  host A — atlas server      │   control plane only (gateway + hub),
            │  --tls-self-signed          │   no local engine; TLS on, prints pin
            │  https/wss endpoint         │
            └─────────────────────────────┘
               ▲ wss:// (dial-out)   ▲ wss:// (dial-out, cross-host)
               │                     │
   ┌───────────┴──────────┐   ┌──────┴───────────────────┐
   │ host A (co-located)  │   │ host B — separate machine │
   │ atlas worker         │   │ atlas worker (GPU)        │
   │ llama.cpp, gguf model│   │ vLLM, qwen3-8b            │
   └──────────────────────┘   └───────────────────────────┘
```

- **Host A** runs `atlas server --tls-self-signed` (no local engine) plus a co-located `atlas worker` serving a **llama.cpp** gguf model. Co-location is fine for the llama.cpp leg — the cross-host proof comes from host B.
- **Host B** is a **separate machine** (the GPU box) running `atlas worker` on **vLLM** (`qwen3-8b`), dialing host A's `wss://` endpoint with the pinned self-signed cert and the join token. This is the leg that proves cross-host dial-out (ADR-0003), wss+TLS across hosts, and "both engines in one fleet".
- One API key authenticates all client traffic to host A; tier aliases resolve to the two models across the two engines.

TLS uses **self-signed + pin** (no domain required — the realistic private-fleet path). ACME (`:443` + public DNS) stays an M5 follow-up and does **not** gate M1; the self-signed/pin path is the M1-relevant transport ([ADR-0009](decisions/0009-transport-security-tls-and-pinning.md)).

## Acceptance criteria

All observed on the topology above, against host A's `https://` endpoint, with workers joined over `wss://`:

1. **Full surface over a remote worker, both engines.** G1–G8 + G10 pass when served by a remote worker over the wss channel — once via the llama.cpp worker, once via the cross-host vLLM worker. This is M0's surface, now transiting a real network rather than loopback.
2. **G11 — multi-worker routing across hosts.** Two workers on different machines, each serving a different model; requests route to the worker that holds the model; when one worker leaves, its model becomes unavailable while the other's keeps serving (connection-identified routes hold across a real disconnect).
3. **G12 — auth over the real endpoint.** Valid key succeeds; missing/invalid key → 401; per-key model allowlist → 403 on a disallowed model; revocation takes effect immediately; an admin-scoped key gates `/admin/*` while a client key → 403.
4. **G13 — usage metering across the fleet.** After the run, the durable ledger has per-model totals with positive token counts, attributed to the correct **remote** worker (by its stable `--name`, not the in-process sentinel), queryable by key/model/worker, surviving a server restart; an interrupted stream still records the tokens emitted up to the cut.
5. **G14 — fleet ops on separate hosts.** `wss://` join works cross-host with a pinned self-signed cert; env-var join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`) works with no CLI flags; SIGTERM drain lets an in-flight request complete while new requests are refused; `kill -9` on a worker unblocks its in-flight requests with a retryable 5xx within the heartbeat timeout (~30 s).
6. **The demo holds.** With both workers joined, both models answer 200 through the one authenticated endpoint simultaneously — server on host A, two engines on two hosts.

## How it is gated

A new **fleet track** in the nightly acceptance workflow ([nightly-gpu.yml](../.github/workflows/nightly-gpu.yml)), parallel to the existing single-node GPU/CPU tracks, provisions host A + host B and runs the criteria above. A green nightly fleet run is what flips M1 to _done_ — the same bar M0 cleared.

What is **already proven per-PR** (so the nightly only needs to add the cross-host/real-hardware dimension): the `conformance-fleet` job on one CI runner covers G1–G8, G10, G11, G13, and G14's drain/timeout/wss-loopback cases on llama.cpp, plus G12 via Go integration tests. The nightly fleet track does not re-prove the logic — it proves it **across machines and on vLLM**.

## Build plan (to reach the green run)

1. **`scripts/acceptance-fleet.sh`** — provider-agnostic stage 2 (mirrors [acceptance.sh](../scripts/acceptance.sh)). Runs on host A: start `atlas server --tls-self-signed`, mint an admin API key, capture the pin + join token, bring up the co-located llama.cpp worker, wait for the expected models (local + the remote GPU worker) to register, run `run.py --require G1..G14` once per engine, then replay the drain / kill-9 / multi-worker-routing scenarios. Worker hosts run a thin `atlas worker --join wss://<hostA> --tls-pin <pin> --token <token>` entrypoint.
2. **Multi-host provisioning in `nightly-gpu.yml`** — the infra crux: two hosts that can reach each other (host B dials host A's wss port), provisioned via the existing `machulav/ec2-github-runner` machinery, with the pin + token + API key handed from host A to host B at launch. Requires a **networking enablement on the project owner's side** (host A reachable by host B — same VPC/security-group with the wss port open, or a public IP), analogous to the M0 AWS OIDC enablement.
3. **Record + flip** — on a green run, mark M1 ✅ done in [roadmap.md](roadmap.md), update this doc's status banner with the run evidence, and trim the M1 "remaining" notes.

## Out of scope for M1-done

- **Apple-Silicon / MLX worker leg** — deferred with the Apple-Silicon runner (open M2 item, [open-questions.md](open-questions.md)).
- **ACME / public-DNS TLS** — self-signed + pin is the M1 transport; ACME `:443` reconciliation is an M5 follow-up ([follow-ups.md](follow-ups.md)).
- **Cross-machine sharding, HA control plane** — never in M1's scope ([roadmap.md](roadmap.md)).
  </content>
