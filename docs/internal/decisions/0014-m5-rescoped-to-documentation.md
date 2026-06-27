# ADR-0014: M5 re-scoped to documentation; packaging & IaC deferred to M7

## Status

accepted

## Context

[roadmap.md](../../roadmap.md) scoped **M5 as "Packaging & deployment"**: a Docker Compose file,
systemd units, Kubernetes manifests, and reference AWS Terraform under `examples/`, plus the
deployment/operations docs to make a team deployment turnkey. The intent was sound when written, but
the ground has shifted by the time M5 comes up:

- **M4 already shipped everything needed to _deploy_ Atlas.** The static binary, `install.sh`, signed
  releases, and the GHCR Docker images all exist ([ADR-0006](0006-packaging-and-deployment.md), M4).
  The worker join flow, drain, and TLS modes ([ADR-0009](0009-transport-security-tls-and-pinning.md))
  are in place. There is no missing capability blocking a real deployment — only missing _packaging
  convenience_ and missing _docs_.
- **The AWS reference _topology_ is already written** ([deployment-aws.md](../../deployment-aws.md)).
- **Compose/systemd/k8s/Terraform recipes built now are speculative.** Atlas has no users yet (the
  repo is not even public — M4's go-live is owner-gated). Committing to a specific compose shape, a
  k8s manifest set, and a Terraform module _before any operator tells us how they actually deploy_
  risks building the wrong abstractions and then carrying their maintenance. Packaging is best pulled
  by real demand, not pushed ahead of it.
- **The acute gap is discoverability, not deployability.** As the project goes public, a newcomer has
  no curated front door: `docs/` mixes user-facing guides with internal milestone scaffolding (build
  plans, acceptance reports, ADRs), and there is no hosted docs site. That is what stands between
  "the machinery works" and "a stranger can adopt it."

This is a load-bearing scope change to a roadmap milestone, so it is recorded here per
[CLAUDE.md](../../../CLAUDE.md) rule 1, with the roadmap updated in the same change.

## Decision

1. **M5 is re-scoped to "Documentation & docs site."** M5 stands up a polished public documentation
   site (Astro Starlight on GitHub Pages at `orchestra-hq.github.io/atlas`) and curates the project's
   user-facing docs onto it, so a newcomer can follow install → quickstart → deploy → operate without
   reading internal design docs. The site _documents_ deploying with what M4 already ships; it adds no
   new packaging artifacts.

2. **The original M5 packaging deliverables are deferred to a new demand-driven M7 — "Packaging &
   IaC."** Compose file, systemd units, k8s manifests (still "packaging only, no first-party operator
   or CRDs" per [ADR-0006](0006-packaging-and-deployment.md)), and the reference AWS Terraform
   (~100-line bar) move to M7, revisited by traction in the spirit of M3's deferred bundle. M6 (web
   console) keeps its slot. The two TLS follow-ups in [follow-ups.md](../follow-ups.md) (self-signed
   SAN-mismatch handling, ACME `:443` reconciliation) "bite on real public deploys," so they ride to
   M7 rather than M5 — no TLS code change in the documentation milestone.

3. **`docs/` is restructured, never deleted.** Internal design truth — ADRs (`decisions/`),
   `m*-build-plan.md`, `m*-acceptance.md`, `research/`, `open-questions.md`, `follow-ups.md` — moves
   under **`docs/internal/`** (via `git mv`, history preserved); it remains the CLAUDE.md-mandated
   design-truth home, just namespaced away from the newcomer path. User-facing prose moves into the
   site as its single canonical home (also via `git mv`). One canonical home per document; nothing is
   stubbed or removed.

4. **The site is GitHub-Pages-only, no custom domain, no external host.** A CI workflow builds the
   site per-PR (machinery proven continuously, M4-style) and deploys on `main`. Going live is an
   owner-gated flip (enable Pages → Source: GitHub Actions), like M4's repo-public flip. A custom
   domain stays a later CNAME-only change, consistent with M4's installer-domain call.

## Consequences

- The public launch leads with a real front door: a hosted, searchable docs site rather than a folder
  of mixed-altitude Markdown. Adoption friction drops where it actually bites.
- Packaging work is not lost — it is parked in M7 with its rationale, and pulled when an operator's
  real deployment shape tells us which abstractions to build. We avoid maintaining speculative
  compose/k8s/Terraform surfaces with no users.
- `docs/internal/` namespacing touches every cross-reference to the moved files — other docs, the
  README map, and ~20 Go doc-comments plus scripts/workflows/`.goreleaser.yaml`/`install.sh`/catalog
  that cite ADR and doc paths. Those references are rewritten in the same restructure change so code
  and docs do not diverge (CLAUDE.md).
- ADR-0006's packaging stance is unchanged (single binary + images, reference IaC under `examples/`,
  no operator). M5/M7 only re-time _when_ the reference IaC and packaging recipes land.
- Revisit M7's priority by traction: if early adopters converge on one deployment shape (e.g. k8s),
  that shape's manifest jumps the queue; if they all use the binary + user-data, M7 may stay small.
