# Roadmap

Phased so every milestone ends in something demoable and marketable. Dates intentionally absent until M0 scoping; order matters more than timing.

## M0 — Single-node MVP: "Claude Code on your own box"

**Status: ✅ done (2026-06-25).** All acceptance criteria green on both engines — vLLM (GPU) and llama.cpp (CPU) — with the real Claude Code drop-in proven on vLLM. See the result banner in [m0-acceptance.md](internal/m0-acceptance.md).

**Demo:** install one binary, `atlas up`, `atlas pull <model>`, point Claude Code at `http://localhost:9090`, complete a real coding task on a local open model. Definition of done: [m0-acceptance.md](internal/m0-acceptance.md). Build order: [m0-build-plan.md](internal/m0-build-plan.md).

- `atlas` CLI + daemon (`up`, `pull`, `run`, `ps`, `serve` equivalents)
- One engine adapter per platform class to start: **llama.cpp** (works everywhere, incl. dev laptops) and **vLLM** (CUDA, the credibility path) — MLX and SGLang follow
- Model store (content-addressable cache) + a starter catalog of 3–5 agent-tested models with correct templates/tool parsers, incl. ≥1 reasoning-capable model (ADR-0005) — candidates in [research/model-catalog-m0.md](internal/research/model-catalog-m0.md)
- **Anthropic `/v1/messages`** incl. streaming + tool use + thinking-block mapping for reasoning models (ADR-0005), `count_tokens`, `/v1/models`; **OpenAI `/v1/chat/completions`** incl. streaming + tools
- Gateway-side context-window assertion: requests that don't fit the resolved model's window are rejected pre-dispatch with a clean Anthropic-shaped 400 ([m0-acceptance.md](internal/m0-acceptance.md))
- Model alias mapping (`claude-* → local model`)
- Conformance suite v0 (real Anthropic + OpenAI SDKs, tool loop, Claude Code smoke test) — specced in [conformance-suite.md](internal/conformance-suite.md)
- Single shared-secret auth for the endpoint
- `/healthz` + `/readyz` endpoints; single-directory state ([deployment requirements](internal/deployment-aws.md#product-requirements-this-topology-imposes))

Cut from M0: web console, multi-node, API key management, usage metering (log counts only).

## M0.5 — Release & prove: "Installable, and proven on a real GPU"

**Status: ✅ done (2026-06-25).** The scheduled nightly acceptance run is green on both engines (vLLM on an on-demand GPU box, llama.cpp on a CPU box); the green GPU run is what flipped M0 to _done_. Docker images, the deploy recipes, and the acceptance machinery are all in place.

**Demo:** `docker run … ghcr.io/orchestra-hq/atlas` on a laptop, or `sky launch atlas-serve.yaml` to bring up Atlas on a cheapest-available cloud GPU, then point Claude Code at it; meanwhile a nightly job proves the full suite green on both engines. Closed out M0: a green GPU acceptance run is what flipped M0 to _done_. Rationale + survey: [research/distribution-deployment-and-gpu-ci.md](internal/research/distribution-deployment-and-gpu-ci.md); decisions: [ADR-0006](internal/decisions/0006-packaging-and-deployment.md). The polished public-install channels (one-line installer, Homebrew) are deliberately deferred to [M4](#m4--deliverability-the-frictionless-install).

- **Docker images** to GHCR (pulled forward from M2): one image, role by subcommand; slim + CUDA "batteries-included" (vLLM) variants
- **Nightly GPU acceptance** — SkyPilot recipe from a CPU runner spins up a spot GPU, runs `G1–G10` + the real Claude Code smoke (`CONF_CLAUDE_CODE_SMOKE=1`) on **llama.cpp and vLLM**, tears down (machulav/ephemeral-EC2 as fallback). This is the deferred capable/GPU tier from [open-questions.md](internal/open-questions.md)
- **Deploy recipes:** the SkyPilot one-command cloud-GPU serve recipe (canonical) + the boring "single GPU box + SSH tunnel from your laptop" path
- **Usage-scenarios doc:** persona → path (laptop / single cloud GPU / dial-out fleet)

Signed GitHub Releases already come from GoReleaser; the frictionless public-install layer (one-line installer + Homebrew tap) is split out to [M4](#m4--deliverability-the-frictionless-install) so M0.5 stays "installable enough to prove it on a GPU" without taking on owned-domain/tap-repo setup.

SkyPilot is fenced to one CI workflow + one optional `examples/` recipe; the Atlas binary never depends on it (ADR-0006).

## M1 — Fleet: "Join three machines, one endpoint"

**Status: ✅ done (2026-06-25).** The multi-host fleet acceptance run is green — `atlas server` on one host + a cross-host vLLM worker on a separate GPU host, G1–G14 across the two machines (full surface over the wss channel, multi-worker routing, auth, usage attribution, drain/timeout). See [m1-acceptance.md](internal/m1-acceptance.md).

**Demo:** `atlas server` on a VPS; `atlas worker --join <url> --token <token>` on a 4090 box and a Mac; deploy two models; one authenticated endpoint serves both. Build order: [m1-build-plan.md](internal/m1-build-plan.md).

- `atlas server` (control plane only) + `atlas worker --join` (outbound WebSocket channel — ADR-0007)
- Worker join (token), heartbeats, drain/remove; hardware inventory reported on join
- Scheduler v1: VRAM-fit placement, manual deploy/scale/stop, auto-start + idle-stop
- Request routing/proxying with streaming across the worker channel
- API key management (create/revoke, per-key model allowlist); replaces M0 shared secret
- Usage metering (tokens by key/model/worker) + `atlas usage` CLI
- TLS for the server endpoint (ACME for public VPS, self-signed for private)
- Cloud-fleet behaviors: non-interactive join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`), graceful drain on SIGTERM, heartbeat-timeout removal ([deployment requirements](internal/deployment-aws.md#product-requirements-this-topology-imposes))

## M2 — Operate: "Run a real fleet from the terminal"

**Status: ✅ done (2026-06-26).** Observability (`/metrics`, `atlas status`/`top`) and load-balancing/backpressure (least-in-flight, bounded admission queue, retryable 429/529) are proven per-PR; the engine-breadth acceptance is green on real hardware — MLX on an Apple-Silicon runner and SGLang on a GPU box both pass `G1–G8,G10` and feed the agent-capability matrix. See [m2-acceptance.md](internal/m2-acceptance.md).

**Demo:** SSH to the gateway box and `atlas top` to watch the fleet live; push concurrent load past capacity and watch requests queue then shed with clean 429/529 instead of timing out; add an Apple-Silicon worker running MLX. Build order: [m2-build-plan.md](internal/m2-build-plan.md).

- Observability: Prometheus `/metrics` endpoint + structured logs
- CLI inspection tool — `atlas status` (snapshot) + `atlas top` (live view), run over SSH on the gateway; the web console's stand-in (the console itself is its own later milestone, [M6](#m6--web-console))
- Load balancing across replicas (least-in-flight) + bounded queueing/backpressure with retryable 429/529 ([ADR-0010](internal/decisions/0010-load-balancing-and-backpressure.md))
- MLX (Apple Silicon) then SGLang (NVIDIA) engine adapters; engine version pinning/upgrade flow
- Catalog expansion + published agent-capability matrix per model; apply the catalog's recorded-but-unused per-model sampling + reasoning config

Web console and packaging/IaC, which earlier drafts had in M2, are split out to their own milestones ([M6](#m6--web-console), [M7](#m7--packaging--iac-by-traction)): operating from the CLI defers the GUI, and packaging is a large independent body of ops work.

## M3 — Ecosystem & differentiation deepeners (pick by traction)

**Status: ✅ done (2026-06-26).** The four chosen threads — prefix/session-affinity routing, embeddings + reranker model classes, the control-plane audit log, and cloud-fallback passthrough — shipped and are proven end-to-end: the per-PR `Conformance (M3)` job (G19–G22) is green on a real two-process llama.cpp deployment. The two heaviest candidates (full HA control plane, hosted offering) stay deferred. See [m3-acceptance.md](internal/m3-acceptance.md).

**Demo:** run a multi-turn agent against the fleet and watch each turn stick to its warm replica; deploy an embedding + a reranker model and point a RAG stack's OpenAI client at the same endpoint; flip on cloud-fallback and watch a capacity spike spill to a provider, clearly labeled, instead of shedding. Build order: [m3-build-plan.md](internal/m3-build-plan.md).

Of the candidate threads below, M3 takes the four that compound M2's runtime depth and serve the self-hosted-agent thesis, and defers the two heaviest (full HA, and the hosted offering):

- **Prefix/session-affinity routing** (agent conversations stick to a warm worker — SGLang prefix-cache synergy). Extends [ADR-0010](internal/decisions/0010-load-balancing-and-backpressure.md) as a load-bounded hint; [ADR-0011](internal/decisions/0011-prefix-session-affinity-routing.md).
- **Embeddings + reranker model classes** as first-class citizens (`/v1/embeddings`, `/v1/rerank`); a model-`class` abstraction; [ADR-0012](internal/decisions/0012-embeddings-and-reranker-model-classes.md).
- **Audit log** of control-plane mutations (split out of the HA bundle — light, independent, high trust value); reuses the [ADR-0008](internal/decisions/0008-control-plane-persistence-and-api-keys.md) store.
- **Cloud-fallback passthrough** (route overflow to a real provider key, clearly labeled, separately billed, off by default); [ADR-0013](internal/decisions/0013-cloud-fallback-passthrough.md).

Deferred from M3 (revisit by traction):

- **HA control plane** (Postgres, durable admission queue, multi-replica) — heavy ops infra; ADR-0008/ADR-0010 left the doors open, so it becomes its own future milestone when demand appears.
- **Hosted control-plane offering** (the open-core conversation) — a business decision, not a build task.

## M4 — Deliverability: "the frictionless install"

**Status: ✅ done (2026-06-27).** The deliverability machinery — `install.sh`, cosign keyless signing, and the Homebrew formula pushed to `orchestra-hq/homebrew-tap` via a dedicated Release App — is built and proven end-to-end by the `v0.1.0` release run. The public channels (`brew install` / `curl | sh`) light up on the owner's go-live flip (repo public + publish the draft); see [m4-build-plan.md](internal/m4-build-plan.md).

**Demo:** a newcomer runs `brew install orchestra-hq/tap/atlas` (or `curl -fsSL <install.sh> | sh`) and is serving a model in one command. Build order + decisions: [m4-build-plan.md](internal/m4-build-plan.md). Until M4 the binary is installed from GitHub Releases / the container image (M0.5, [ADR-0006](internal/decisions/0006-packaging-and-deployment.md)).

- **Homebrew tap** — a public, reusable `orchestra-hq/homebrew-tap` repo; GoReleaser publishes the formula on each release, pushing via a dedicated "Atlas Release" GitHub App (short-lived token, no PAT).
- **One-line installer** (`install.sh`): detects OS/arch, fetches the pinned signed release, verifies checksum + cosign signature, drops `atlas` on `PATH`. Served from the repo / GitHub Releases — **no custom domain** (owner's call, 2026-06-26); a vanity URL can be added later as a doc-only change.
- **Release signing** with cosign keyless (GitHub OIDC, no key to manage), so both channels verify what they download.
- Install/upgrade UX polish: `atlas --version` upgrade hint, scriptable non-interactive install, documented verification.
- Optional Linux packaging (`.deb`/`.rpm`, GoReleaser nfpm) deferred until there's demand.
- **Public go-live is gated on the `atlas` repo going public** (the release binaries must be anonymously downloadable); the machinery is built + snapshot-validated ahead of that.

## M5 — Documentation & docs site

**Re-scoped 2026-06-27** ([ADR-0014](internal/decisions/0014-m5-rescoped-to-documentation.md)) from "Packaging & deployment" to documentation. M4 already shipped everything needed to _deploy_ Atlas (binary, `install.sh`, signed releases, GHCR images — [ADR-0006](internal/decisions/0006-packaging-and-deployment.md)) and the AWS reference _topology_ is already written ([deployment-aws.md](internal/deployment-aws.md)); the acute gap as the project goes public is **discoverability, not deployability**. Building compose/k8s/Terraform recipes ahead of any operator telling us how they actually deploy is speculative, so that packaging work is deferred to a demand-driven [M7](#m7--packaging--iac-by-traction). M5 instead stands up a curated public docs site so a newcomer can go install → quickstart → deploy → operate without reading the internal design docs.

**Demo:** a newcomer lands on `orchestra-hq.github.io/atlas`, follows Get started → Deploy → Operate, and is serving a model — never touching the repo's internal scaffolding. Build order: [m5-build-plan.md](internal/m5-build-plan.md).

- **Docs site:** Astro Starlight, deployed to **GitHub Pages** (`orchestra-hq.github.io/atlas`); build per-PR, deploy on `main`. No custom domain (a later CNAME-only change, as with M4's installer).
- **Curated user-facing content:** Get started (install · quickstart), Guides (Claude Code · Agent SDK · OpenAI/LangChain · models & catalog), Deploy (laptop · single GPU box · Docker · cloud fleet / AWS reference topology · hybrid — all grounded in M4-shipped artifacts, **no new packaging**), Operate (keys · usage · observability · TLS · drain/scale), Reference (CLI · API compatibility · config), About (vision · positioning).
- **Restructure, not deletion:** internal design truth (ADRs, build plans, acceptance reports, research, open-questions, follow-ups) moves under `docs/internal/`; user-facing prose moves into the site. One canonical home per doc; nothing removed.
- **Go-live** is an owner-gated flip: enable GitHub Pages (Source: GitHub Actions), like M4's repo-public flip.

## M6 — Web console

The graphical operate surface, held until the very end: M2's `atlas status`/`atlas top` CLI covers "see what the fleet is doing" from the terminal, so the console is a convenience layer, not a prerequisite. The SPA-vs-separate-service architecture decision is made when this milestone starts (it needs its own ADR then).

- Web console (workers, models, instances, keys, usage) served by `atlas server`, gated by the existing admin-scoped API key
- Read-only dashboards first (consuming M2's `/metrics` + admin read APIs), then write actions (deploy/scale/stop, key management) through the existing admin endpoints
- SSO for the console (moved here from M3, since it presupposes the console exists)

## M7 — Packaging & IaC (by traction)

The deploy-recipe and packaging surface, **deferred here from M5** ([ADR-0014](internal/decisions/0014-m5-rescoped-to-documentation.md)) so it is pulled by real demand rather than pushed ahead of any users. The Docker images already ship in M0.5 ([ADR-0006](internal/decisions/0006-packaging-and-deployment.md)) and M5 documents deploying with what M4 ships; M7 adds the convenience packaging on top — prioritized by whichever deployment shape early adopters actually converge on.

- Packaging: compose file, systemd units, k8s manifests (packaging only — no first-party operator/CRDs, per [ADR-0006](internal/decisions/0006-packaging-and-deployment.md))
- Reference IaC under `examples/` — AWS Terraform first (~100-line bar, see [deployment-aws.md](internal/deployment-aws.md)); `s3://` model sources
- TLS/ACME hardening on a real public deployment, resolving the remaining transport-security follow-ups in [follow-ups.md](internal/follow-ups.md): self-signed cert host-change handling, ACME `:443` reconciliation

## Standing tracks (every milestone)

- **Docs & marketing:** each milestone ships with a polished guide + demo video; recipes for Claude Agent SDK, Claude Code, OpenAI Agents SDK, LangChain.
- **Conformance:** compat matrix published and CI-enforced; breakage of `ANTHROPIC_BASE_URL` drop-in is a release blocker.
- **Catalog:** model testing is continuous; "works for agents" badge is earned by the suite, not vibes.
