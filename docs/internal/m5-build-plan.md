# M5 build plan

> **Re-scoped 2026-06-27** ([ADR-0014](decisions/0014-m5-rescoped-to-documentation.md)): M5 changed from "Packaging & deployment" to **documentation**. M4 already shipped what is needed to _deploy_ Atlas (binary, `install.sh`, signed releases, GHCR images) and the AWS reference topology is already written; the acute gap as the project goes public is **discoverability, not deployability**. The packaging/IaC work (compose, systemd, k8s, Terraform) is deferred to a demand-driven [M7](../roadmap.md#m7--packaging--iac-by-traction). M5 stands up a curated public docs site instead.

The path from "the machinery works" to **"a stranger can adopt it"**: a newcomer finds install →
quickstart → deploy → operate on a polished docs site, without reading the repo's internal design
docs. M5 refines the M5 milestone in [roadmap.md](../roadmap.md); the packaging/distribution decisions
it builds on live in [ADR-0006](decisions/0006-packaging-and-deployment.md) and the re-scope rationale
in [ADR-0014](decisions/0014-m5-rescoped-to-documentation.md).

M5 adds **no new packaging artifacts** and **no new API surface**. It is a documentation milestone:
a docs site, the content curated onto it, and a clean split of `docs/` into a public front door
(the site) and an internal design-truth home (`docs/internal/`).

## Build-time technical decisions

Choices recorded here so they don't get re-litigated mid-build:

1. **Astro Starlight, in-repo under `website/`.** The site source lives in the `atlas` repo so docs
   version with the code. Starlight content lives in `website/src/content/docs/`, authored as
   Markdown/MDX. Starlight chosen over MkDocs Material (which entered maintenance mode in Nov 2025)
   and Docusaurus (heavier React surface): it is actively developed, fast (zero-JS by default), has
   built-in search, and reuses the existing Markdown.
2. **Restructure, never delete — one canonical home per doc.** `docs/` splits cleanly. Internal
   design truth — all ADRs (`decisions/`), `m*-build-plan.md`, `m*-acceptance.md`, `research/`,
   `open-questions.md`, `follow-ups.md` — moves under **`docs/internal/`** via `git mv` (history
   preserved); it stays the [CLAUDE.md](../../CLAUDE.md)-mandated design-truth home, just namespaced.
   User-facing prose moves into the site (also `git mv`) as its single canonical home — curated for a
   public audience, but moved, not copied-then-deleted. Every reference to a moved file is rewritten
   in the same change (other docs, the README map, and ~20 Go doc-comments plus
   scripts/workflows/`.goreleaser.yaml`/`install.sh`/catalog that cite ADR and doc paths) so code and
   docs don't diverge.
3. **GitHub Pages via Actions; build per-PR, deploy on `main`.** A `docs-site.yml` workflow runs the
   site build + a link check on every PR touching `website/` (machinery proven per-PR, M4-style), and
   deploys to Pages only on `main`. Going live needs the owner to **enable Pages (Source: GitHub
   Actions)** once — the M5 equivalent of M4's owner flip. Everything else is buildable/reviewable
   without that.
4. **No custom domain, no external host.** GitHub Pages only; `orchestra-hq.github.io/atlas`. Astro
   `base: '/atlas'` set accordingly. A CNAME can be added later with no content change, consistent
   with M4's installer-domain call.
5. **Ship an `llms.txt`.** Atlas is LLM infrastructure; an `llms.txt`/`llms-full.txt` (via the
   `starlight-llms-txt` plugin) is cheap, on-theme, and makes the docs agent-consumable. Folded into
   polish, not a blocker.

## Phases

Each phase is one PR, green-only, squash-merged via `scripts/ship.sh`. The Phase 1 work is split into
three reviewable PRs (re-scope / restructure / scaffold) because the restructure touches source files.

| Phase | Deliverable                                        | Exit criterion                                                                                                                                                                                                                                                     |
| ----- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1a    | Re-scope: ADR-0014 + roadmap + this build plan     | ADR-0014 accepted; roadmap renames M5 → docs and adds M7; this doc exists                                                                                                                                                                                          |
| 1b    | Restructure design truth into `docs/internal/`     | ADRs + build plans + acceptance + research + open-questions + follow-ups `git mv`'d under `docs/internal/`; every reference (docs, README, ~20 Go files, scripts, workflows, `.goreleaser.yaml`, `install.sh`, catalog) rewritten; `bash scripts/check.sh` green   |
| 1c    | Docs-site scaffold + CI                            | `website/` Starlight project builds locally; `docs-site.yml` builds on PR and is wired to deploy to Pages on `main`; landing page + sidebar groups render                                                                                                          |
| 2     | Information architecture + getting-started content | Site IA fixed; install, quickstart, Claude-Code/SDK drop-in, API-compatibility, and model-catalog pages `git mv`'d into the site and curated                                                                                                                       |
| 3     | Deploy + operate docs                              | A "Deploy" section per persona path (laptop, single GPU box, Docker, cloud fleet / AWS reference topology, hybrid) **using only M4 artifacts**, plus an "Operate" section (keys, usage, observability/metrics, TLS modes, drain/scale). No new packaging artifacts |
| 4     | Polish + reconcile + go-live                       | Search, 404, edit-on-GitHub links, `llms.txt`; README doc-map + dropped refs reconciled; owner enables Pages → site live — **M5 done**                                                                                                                             |

Exit criteria are cumulative. Phases 1–3 are fully buildable/reviewable without owner action; phase 4
go-live is the owner-gated Pages flip.

## Phase notes

**Phase 1a — re-scope.** [ADR-0014](decisions/0014-m5-rescoped-to-documentation.md) records the change;
[roadmap.md](../roadmap.md) renames M5 → "Documentation & docs site" and adds **M7 — Packaging & IaC (by
traction)** after M6 with the old M5 packaging bullets. Doc-only, small.

**Phase 1b — restructure.** `git mv` the design-truth docs under `docs/internal/` and rewrite every
reference so nothing diverges (CLAUDE.md). Re-home the two TLS follow-ups' **Suggested** line in
`docs/internal/follow-ups.md` to M7. Source-touching but mechanical; gated by `bash scripts/check.sh`.

**Phase 1c — scaffold.** Stand up the Starlight project under `website/` (pinned versions), set the
`site` and `base: '/atlas'` config, and add a landing page plus the top-level sidebar groups. Add
`.github/workflows/docs-site.yml`: the PR job builds the site + runs a link check on changes under
`website/`; the `main` job deploys via `actions/deploy-pages`. Add `website/node_modules` and the
Astro build output to `.gitignore`.

**Phase 2 — IA + getting started.** Lock the sidebar: **Home**, **Get started** (install · quickstart
· verify), **Guides** (Claude Code · Claude Agent SDK · OpenAI SDK/LangChain · models & catalog),
**Deploy** (phase 3), **Operate** (phase 3), **Reference** (CLI · API compatibility · config/env),
**About** (vision · positioning). Migrate getting-started + reference content: install/quickstart from
`README.md` + `install.sh`; API compatibility from `api-surface.md`; Docker from `docker.md`; persona
paths from `usage-scenarios.md`. Each user-facing `docs/*.md` is `git mv`'d into the site (history
preserved) and curated — moved, not stubbed.

**Phase 3 — deploy + operate.** Turn `deployment-aws.md` into a proper **Deploy** section: one page per
path (laptop/`atlas up`, single GPU box + SSH tunnel, Docker slim/cuda, cloud fleet via the AWS
reference topology + ASG/spot/user-data join, and the hybrid control-plane-here/workers-anywhere
story), each grounded only in M4-shipped artifacts. Add an **Operate** section: API keys, usage
metering (`atlas usage`), observability (`/metrics`, `atlas status`/`top`), TLS modes (ACME / supplied
cert / self-signed + pin, per [ADR-0009](decisions/0009-transport-security-tls-and-pinning.md)), and
drain/scale. State plainly that compose/systemd/k8s/Terraform recipes are the future M7.

**Phase 4 — polish + go-live.** Enable Starlight search, a 404 page, GitHub edit links, and `llms.txt`.
Reconcile [README.md](../../README.md)'s documentation map to point users at the site and contributors at
`docs/internal/`; fix any remaining dropped `get.atlas.dev` refs. Final pass that no user-facing topic
lingers under `docs/`. Owner enables GitHub Pages → verify the live site (nav, search, every roadmap
user path present).

## Acceptance — what "M5 done" means

M5 has no new conformance G-group (it is documentation, not API surface), like M4. "Done" is:

- The site is **live** at `orchestra-hq.github.io/atlas`, built and deployed by CI from `main`.
- The four content sections (Get started, Guides, Deploy, Operate) plus Reference are populated; every
  user-facing path named in the roadmap is reachable from the site nav.
- User-facing content lives **only** on the site; `docs/internal/` holds the design truth repo-only.
  Nothing was deleted — everything moved with history.
- The site build + link check run green per-PR (the continuously-proven machinery).

## Who does what

**Owner (one-time):** enable **GitHub Pages → Source: GitHub Actions** on the `atlas` repo (the
go-live flip). Optional/later: register a docs domain (CNAME-only change).

**Claude (everything in-repo):** ADR-0014 + roadmap/follow-ups updates; the `docs/internal/`
restructure + reference rewrites; the `website/` Starlight project; `docs-site.yml`; all content
migration/curation; README reconciliation; this build plan.

## Out of scope for M5

- **Packaging artifacts** (compose, systemd, k8s manifests, reference Terraform) — deferred to
  [M7](../roadmap.md#m7--packaging--iac-by-traction).
- **TLS/ACME code changes** (self-signed SAN-mismatch, ACME `:443`) — they bite on real public
  deploys; tracked in [follow-ups.md](follow-ups.md), suggested for M7.
- **Custom/owned docs domain** — `github.io` for now; a later CNAME-only change.
- **Web console** — that is [M6](../roadmap.md#m6--web-console).
