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

### 6. Existing app conformance inputs

Will prefers not to point Atlas work at the app codebase. Instead, he'll answer a short questionnaire about its API usage, which becomes the M0 acceptance scope:

1. Does it call the Messages API directly, or run the **Claude Agent SDK** harness? (The harness implies: streaming, client-side tool loop, system prompts, `count_tokens`, `cache_control` traffic.)
2. Any **server-side tools** (web search, code execution)? These don't exist on Atlas — if used, the app needs client-side equivalents before redirection.
3. Which **model tiers** (opus/sonnet/haiku) does it use, and roughly what context length per request? (Drives the alias mapping and the GPU/model sizing.)
4. **Multimodal** inputs (images/PDFs)?
5. Peak **concurrency** — parallel agent sessions / requests per minute? (Drives engine choice and replica planning.)
6. Anything using **batches, files, or thinking** explicitly?

## Resolved

- **Language: Go** — accepted by Will 2026-06-12. ADR-0004 flipped to `accepted`.
- **Scope guard: no cross-machine model sharding** — confirmed by Will 2026-06-12. exo-style sharding stays out of scope.
- **Terraform/IaC as a product surface: no** — confirmed by Will 2026-06-12. We ship *reference* IaC examples as documentation (M2), never a supported Terraform product. See [deployment-aws.md](deployment-aws.md).
