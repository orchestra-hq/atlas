# Roadmap

Phased so every milestone ends in something demoable and marketable. Dates intentionally absent until M0 scoping; order matters more than timing.

## M0 — Single-node MVP: "Claude Code on your own box"

**Demo:** install one binary, `atlas up`, `atlas pull <model>`, point Claude Code at `http://localhost:9090`, complete a real coding task on a local open model.

- `atlas` CLI + daemon (`up`, `pull`, `run`, `ps`, `serve` equivalents)
- One engine adapter per platform class to start: **llama.cpp** (works everywhere, incl. dev laptops) and **vLLM** (CUDA, the credibility path) — MLX and SGLang follow
- Model store (content-addressable cache) + a starter catalog of 3–5 agent-tested models with correct templates/tool parsers
- **Anthropic `/v1/messages`** incl. streaming + tool use, `count_tokens`, `/v1/models`; **OpenAI `/v1/chat/completions`** incl. streaming + tools
- Model alias mapping (`claude-* → local model`)
- Conformance suite v0 (real Anthropic + OpenAI SDKs, tool loop, Claude Code smoke test)
- Single shared-secret auth for the endpoint

Cut from M0: web console, multi-node, API key management, usage metering (log counts only).

## M1 — Fleet: "Join three machines, one endpoint"

**Demo:** `atlas server` on a VPS; `atlas worker --join <token>` on a 4090 box and a Mac; deploy two models; one authenticated endpoint serves both.

- Worker join (token), persistent outbound channel, heartbeats, worker drain/remove
- Scheduler v1: VRAM-fit placement, manual deploy/scale/stop, auto-start + idle-stop
- Request routing/proxying with streaming across the worker channel
- API key management (create/revoke, per-key model allowlist)
- Usage metering (tokens by key/model/worker) + `atlas` CLI views
- TLS story for the server endpoint

## M2 — Platform: "Private LLM service your team actually operates"

- Web console (workers, models, instances, keys, usage)
- Replicas + load balancing across instances; basic queueing/backpressure with sane 429/529 behavior
- SGLang + MLX adapters; engine version pinning/upgrade flow
- Catalog expansion + published agent-capability test results per model
- Packaging: Docker images, compose file, systemd units, (k8s manifests as packaging only)
- Observability: Prometheus metrics endpoint, structured logs

## M3 — Ecosystem & differentiation deepeners (pick by traction)

- Embeddings + reranker model classes as first-class citizens
- Thinking-tokens mapping for reasoning models (DeepSeek-R1-style) onto Anthropic thinking blocks
- Prefix/session-affinity routing (agent conversations stick to a warm worker — SGLang synergy)
- Cloud-fallback passthrough (route overflow to a real provider key, clearly labeled)
- HA control plane; audit log; SSO for console
- Hosted control plane offering (the open-core conversation — separate decision)

## Standing tracks (every milestone)

- **Docs & marketing:** each milestone ships with a polished guide + demo video; recipes for Claude Agent SDK, Claude Code, OpenAI Agents SDK, LangChain.
- **Conformance:** compat matrix published and CI-enforced; breakage of `ANTHROPIC_BASE_URL` drop-in is a release blocker.
- **Catalog:** model testing is continuous; "works for agents" badge is earned by the suite, not vibes.
