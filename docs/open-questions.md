# Open questions

Decisions that need Will's call (or at least sign-off). Resolved items move to the bottom with their outcome.

## Open

### 1. Name

"Atlas" is a placeholder and collides widely. Will agrees it's not unique and will come back with ideas. Keep `atlas` internally; rename before anything public. **Waiting on Will.**

### 2. License

**On hold by Will's call (2026-06-12): decide when we go public.** Recommendation on file remains Apache 2.0. Nothing blocks on this until first public release; it becomes a hard blocker at that point (and before accepting external contributions).

### 3. M0 engine pair

Proposed: llama.cpp (universal dev experience) + vLLM (CUDA credibility) both in M0. Not yet explicitly confirmed.

### 4. How do workers get engine runtimes?

Containers where available, managed venv (uv) fallback for bare metal — recommendation (c). Decide at M0 build time.

### 5. Open-core / monetization posture

Defer, but document intent before first external contribution.

### 6. App context-window strategy

The app sends "any length of request", but open models top out at 32k–256k context vs Claude's 1M. Atlas will report real windows via `/v1/models` and return clean SDK-parseable errors — but whether the app handles smaller windows gracefully or we provision long-context models for the opus alias is Will's call when deployment nears. See [m0-acceptance.md](m0-acceptance.md). **Parked until app deployment planning.**

## Resolved

- **Language: Go** — accepted by Will 2026-06-12. ADR-0004 flipped to `accepted`.
- **Scope guard: no cross-machine model sharding** — confirmed by Will 2026-06-12. exo-style sharding stays out of scope.
- **Terraform/IaC as a product surface: no** — confirmed by Will 2026-06-12. We ship _reference_ IaC examples as documentation (M2), never a supported Terraform product. See [deployment-aws.md](deployment-aws.md).
- **Existing app conformance inputs** — questionnaire answered by Will 2026-06-12; captured as [m0-acceptance.md](m0-acceptance.md). Headline: Agent SDK harness, streaming + client-side tool loop are the non-negotiables; no server-side tools; all tiers; no multimodal/batches/files/thinking today.
