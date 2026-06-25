# CLAUDE.md — guide for agents working in this repo

## What this project is

Atlas is an open source self-hosted LLM inference platform: a control plane + per-machine workers that orchestrate existing inference engines (vLLM, SGLang, llama.cpp, MLX) and expose Anthropic-compatible (`/v1/messages`) and OpenAI-compatible APIs, so agents built on the Claude Agent SDK or OpenAI SDKs can be pointed at user-controlled hardware.

## Current phase

**Build (M2).** M0, M0.5, and M1 are **done** (declared 2026-06-25 — M0/M0.5's GPU acceptance is green on both engines per `docs/m0-acceptance.md`; M1's multi-host fleet acceptance is green per `docs/m1-acceptance.md`). M2 is code-complete; M2 ("operate a real fleet from the terminal") is the active milestone, with code landing in the phase order of `docs/m2-build-plan.md`. Design truth still lives in `docs/`; when code and a design doc diverge, fix one or the other in the same change.

## Rules

1. **Design truth lives in `docs/`.** If you change direction on anything load-bearing (architecture, API shape, language, scope), record it as an ADR in `docs/decisions/` (next sequential number, same format as existing ones) and update the affected docs in the same change.
2. **Don't reinvent inference.** Atlas wraps engines; it does not implement attention kernels, samplers, or model loading. See ADR-0001.
3. **The Anthropic Messages API is the first-class surface.** API changes must preserve drop-in compatibility for the Claude Agent SDK / Claude Code (`ANTHROPIC_BASE_URL` redirection). See ADR-0002 and `docs/api-surface.md`.
4. **Workers dial out, never listen for the control plane.** This is core to the "runs on anyone's infra" promise. See ADR-0003.
5. **Open questions go in `docs/open-questions.md`**, not silently resolved. If you must assume an answer to make progress, state the assumption in your output and add it to that file.
6. **Cite sources in research docs.** `docs/research/` entries should link the project/docs they describe.

## Conventions

- Markdown docs, sentence-case headings, one doc per concern.
- **Changes land via PR, never direct push.** `main` is protected by a repo ruleset: PRs required, all CI checks green, squash-merge only. Branch from `main`, open a PR sized as one logical change (a build-plan phase, a fix), merge when green. Real release artifacts come from `v*` tags (`release.yml`); CI's build/release-dry-run jobs only prove the machinery on every PR.
- **Dev workflow is scripted (`scripts/`).** `bash scripts/check.sh` (or `make check`) is the local pre-flight: it formats Go + Markdown and runs the same gates CI does (vet, lint, test, tidy), with no git/remote side effects — run it freely. `bash scripts/ship.sh "PR title" [body-file]` (or `make ship MSG="…" [BODY=…]`) is the whole PR workflow above as one command: clean → check → commit → push → open PR → squash auto-merge → poll CI → sync `main` + delete the branch. It refuses to run on `main`. `scripts/clean.sh` just stops local e2e processes (`atlas up`, `llama-server`) and deletes ephemeral artifacts, leaving the downloaded model and provisioned runtime in place. Commit-message attribution trailers go in the `ship.sh` body-file, not the scripts.
- **Go:** module `github.com/orchestra-hq/atlas`, layout per `docs/m0-build-plan.md`. `make build / test / lint / fmt / snapshot`; formatting is gofumpt + goimports via `golangci-lint fmt` (config in `.golangci.yml`), releases via GoReleaser (`.goreleaser.yaml`). CI runs on Blacksmith runners (`.github/workflows/ci.yml`); tool versions are pinned there — keep them in sync when upgrading.
- **Formatting & linting:** the pre-commit hook in `.githooks/` runs `prettier --write` (table/emphasis formatting) then `markdownlint-cli2 --fix` on staged `.md` files, re-stages, and fails on remaining errors. Once per clone: `git config core.hooksPath .githooks`. Rules live in `.markdownlint.jsonc` — disabling a rule is fine when deliberate, but comment why. Tables are padded-pipe style (MD060); ASCII diagrams use ` ```text ` fences.
- ADR format: Status / Context / Decision / Consequences. Statuses: `proposed`, `accepted`, `superseded by ADR-XXXX`.
- Keep `README.md`'s documentation map in sync when adding docs.
