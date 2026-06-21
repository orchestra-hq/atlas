# Roadmap

Phased so every milestone ends in something demoable and marketable. Dates intentionally absent until M0 scoping; order matters more than timing.

## M0 — Single-node MVP: "Claude Code on your own box"

**Demo:** install one binary, `atlas up`, `atlas pull <model>`, point Claude Code at `http://localhost:9090`, complete a real coding task on a local open model. Definition of done: [m0-acceptance.md](m0-acceptance.md). Build order: [m0-build-plan.md](m0-build-plan.md).

- `atlas` CLI + daemon (`up`, `pull`, `run`, `ps`, `serve` equivalents)
- One engine adapter per platform class to start: **llama.cpp** (works everywhere, incl. dev laptops) and **vLLM** (CUDA, the credibility path) — MLX and SGLang follow
- Model store (content-addressable cache) + a starter catalog of 3–5 agent-tested models with correct templates/tool parsers, incl. ≥1 reasoning-capable model (ADR-0005) — candidates in [research/model-catalog-m0.md](research/model-catalog-m0.md)
- **Anthropic `/v1/messages`** incl. streaming + tool use + thinking-block mapping for reasoning models (ADR-0005), `count_tokens`, `/v1/models`; **OpenAI `/v1/chat/completions`** incl. streaming + tools
- Gateway-side context-window assertion: requests that don't fit the resolved model's window are rejected pre-dispatch with a clean Anthropic-shaped 400 ([m0-acceptance.md](m0-acceptance.md))
- Model alias mapping (`claude-* → local model`)
- Conformance suite v0 (real Anthropic + OpenAI SDKs, tool loop, Claude Code smoke test) — specced in [conformance-suite.md](conformance-suite.md)
- Single shared-secret auth for the endpoint
- `/healthz` + `/readyz` endpoints; single-directory state ([deployment requirements](deployment-aws.md#product-requirements-this-topology-imposes))

Cut from M0: web console, multi-node, API key management, usage metering (log counts only).

## M0.5 — Release & prove: "Installable, and proven on a real GPU"

**Demo:** `docker run … ghcr.io/orchestra-hq/atlas` on a laptop, or `sky launch atlas-serve.yaml` to bring up Atlas on a cheapest-available cloud GPU, then point Claude Code at it; meanwhile a nightly job proves the full suite green on both engines. Closes out M0: a green GPU acceptance run is what flips M0 to _done_. Rationale + survey: [research/distribution-deployment-and-gpu-ci.md](research/distribution-deployment-and-gpu-ci.md); decisions: [ADR-0006](decisions/0006-packaging-and-deployment.md). The polished public-install channels (one-line installer, Homebrew) are deliberately deferred to [M4](#m4--deliverability-the-frictionless-install).

- **Docker images** to GHCR (pulled forward from M2): one image, role by subcommand; slim + CUDA "batteries-included" (vLLM) variants
- **Nightly GPU acceptance** — SkyPilot recipe from a CPU runner spins up a spot GPU, runs `G1–G10` + the real Claude Code smoke (`CONF_CLAUDE_CODE_SMOKE=1`) on **llama.cpp and vLLM**, tears down (machulav/ephemeral-EC2 as fallback). This is the deferred capable/GPU tier from [open-questions.md](open-questions.md)
- **Deploy recipes:** the SkyPilot one-command cloud-GPU serve recipe (canonical) + the boring "single GPU box + SSH tunnel from your laptop" path
- **Usage-scenarios doc:** persona → path (laptop / single cloud GPU / dial-out fleet)

Signed GitHub Releases already come from GoReleaser; the frictionless public-install layer (one-line installer + Homebrew tap) is split out to [M4](#m4--deliverability-the-frictionless-install) so M0.5 stays "installable enough to prove it on a GPU" without taking on owned-domain/tap-repo setup.

SkyPilot is fenced to one CI workflow + one optional `examples/` recipe; the Atlas binary never depends on it (ADR-0006).

## M1 — Fleet: "Join three machines, one endpoint"

**Demo:** `atlas server` on a VPS; `atlas worker --join <url> --token <token>` on a 4090 box and a Mac; deploy two models; one authenticated endpoint serves both. Build order: [m1-build-plan.md](m1-build-plan.md).

- `atlas server` (control plane only) + `atlas worker --join` (outbound WebSocket channel — ADR-0007)
- Worker join (token), heartbeats, drain/remove; hardware inventory reported on join
- Scheduler v1: VRAM-fit placement, manual deploy/scale/stop, auto-start + idle-stop
- Request routing/proxying with streaming across the worker channel
- API key management (create/revoke, per-key model allowlist); replaces M0 shared secret
- Usage metering (tokens by key/model/worker) + `atlas usage` CLI
- TLS for the server endpoint (ACME for public VPS, self-signed for private)
- Cloud-fleet behaviors: non-interactive join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`), graceful drain on SIGTERM, heartbeat-timeout removal ([deployment requirements](deployment-aws.md#product-requirements-this-topology-imposes))

## M2 — Operate: "Run a real fleet from the terminal"

**Demo:** SSH to the gateway box and `atlas top` to watch the fleet live; push concurrent load past capacity and watch requests queue then shed with clean 429/529 instead of timing out; add an Apple-Silicon worker running MLX. Build order: [m2-build-plan.md](m2-build-plan.md).

- Observability: Prometheus `/metrics` endpoint + structured logs
- CLI inspection tool — `atlas status` (snapshot) + `atlas top` (live view), run over SSH on the gateway; the web console's stand-in (the console itself is its own later milestone, [M6](#m6--web-console))
- Load balancing across replicas (least-in-flight) + bounded queueing/backpressure with retryable 429/529 ([ADR-0010](decisions/0010-load-balancing-and-backpressure.md))
- MLX (Apple Silicon) then SGLang (NVIDIA) engine adapters; engine version pinning/upgrade flow
- Catalog expansion + published agent-capability matrix per model; apply the catalog's recorded-but-unused per-model sampling + reasoning config

Web console and packaging/IaC, which earlier drafts had in M2, are split out to their own milestones ([M6](#m6--web-console), [M5](#m5--packaging--deployment)): operating from the CLI defers the GUI, and packaging is a large independent body of ops work.

## M3 — Ecosystem & differentiation deepeners (pick by traction)

- Embeddings + reranker model classes as first-class citizens
- Prefix/session-affinity routing (agent conversations stick to a warm worker — SGLang synergy)
- Cloud-fallback passthrough (route overflow to a real provider key, clearly labeled)
- HA control plane; audit log
- Hosted control plane offering (the open-core conversation — separate decision)

## M4 — Deliverability: "the frictionless install"

**Demo:** a newcomer runs `brew install atlas` (or `curl get.atlas.dev | sh`) and is serving a model in one command. The polished, owned-channel public install — held until the project is worth installing that frictionlessly, and until the owner is ready to take on the domain + tap-repo upkeep these imply. Until then the binary is installed from GitHub Releases / the container image (M0.5, [ADR-0006](decisions/0006-packaging-and-deployment.md)).

- **One-line installer** at an owned domain (`get.atlas.dev | sh`): detects OS/arch, fetches the pinned signed release, verifies checksums, drops `atlas` on `PATH`. (Needs a domain the owner controls.)
- **Homebrew tap** (`orchestra-hq/homebrew-tap`, GoReleaser-published formula). (Needs a separate tap repo.)
- Install/upgrade UX polish: `atlas --version` self-update hint, scriptable non-interactive install, checksum/signature verification documented.
- Optional Linux packaging (`.deb`/`.rpm`, GoReleaser nfpm) if there's demand.

## M5 — Packaging & deployment

A large, independent body of ops work, deliberately kept out of M2 so the runtime milestone stays focused. The Docker images themselves already ship in M0.5 ([ADR-0006](decisions/0006-packaging-and-deployment.md)); this milestone is the deploy-recipe and packaging surface on top of them, plus the docs that make a team deployment turnkey.

- Packaging: compose file, systemd units, k8s manifests (packaging only — no first-party operator/CRDs, per [ADR-0006](decisions/0006-packaging-and-deployment.md))
- Reference IaC under `examples/` — AWS Terraform first (~100-line bar, see [deployment-aws.md](deployment-aws.md)); `s3://` model sources
- Deployment & operations docs: production topology, TLS/ACME on a real public deployment (resolves the remaining transport-security follow-ups in [follow-ups.md](follow-ups.md): self-signed cert host-change handling, ACME `:443` reconciliation)

## M6 — Web console

The graphical operate surface, held until the very end: M2's `atlas status`/`atlas top` CLI covers "see what the fleet is doing" from the terminal, so the console is a convenience layer, not a prerequisite. The SPA-vs-separate-service architecture decision is made when this milestone starts (it needs its own ADR then).

- Web console (workers, models, instances, keys, usage) served by `atlas server`, gated by the existing admin-scoped API key
- Read-only dashboards first (consuming M2's `/metrics` + admin read APIs), then write actions (deploy/scale/stop, key management) through the existing admin endpoints
- SSO for the console (moved here from M3, since it presupposes the console exists)

## Standing tracks (every milestone)

- **Docs & marketing:** each milestone ships with a polished guide + demo video; recipes for Claude Agent SDK, Claude Code, OpenAI Agents SDK, LangChain.
- **Conformance:** compat matrix published and CI-enforced; breakage of `ANTHROPIC_BASE_URL` drop-in is a release blocker.
- **Catalog:** model testing is continuous; "works for agents" badge is earned by the suite, not vibes.
