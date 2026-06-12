# CLAUDE.md — guide for agents working in this repo

## What this project is

Atlas (working name) is an open source self-hosted LLM inference platform: a control plane + per-machine workers that orchestrate existing inference engines (vLLM, SGLang, llama.cpp, MLX) and expose Anthropic-compatible (`/v1/messages`) and OpenAI-compatible APIs, so agents built on the Claude Agent SDK or OpenAI SDKs can be pointed at user-controlled hardware.

## Current phase

**Design.** There is no application code yet. The source of truth is `docs/`. Do not scaffold code without an explicit request from the project owner (Will).

## Rules

1. **Design truth lives in `docs/`.** If you change direction on anything load-bearing (architecture, API shape, language, scope), record it as an ADR in `docs/decisions/` (next sequential number, same format as existing ones) and update the affected docs in the same change.
2. **Don't reinvent inference.** Atlas wraps engines; it does not implement attention kernels, samplers, or model loading. See ADR-0001.
3. **The Anthropic Messages API is the first-class surface.** API changes must preserve drop-in compatibility for the Claude Agent SDK / Claude Code (`ANTHROPIC_BASE_URL` redirection). See ADR-0002 and `docs/api-surface.md`.
4. **Workers dial out, never listen for the control plane.** This is core to the "runs on anyone's infra" promise. See ADR-0003.
5. **Open questions go in `docs/open-questions.md`**, not silently resolved. If you must assume an answer to make progress, state the assumption in your output and add it to that file.
6. **Cite sources in research docs.** `docs/research/` entries should link the project/docs they describe.

## Conventions

- Markdown docs, sentence-case headings, one doc per concern.
- ADR format: Status / Context / Decision / Consequences. Statuses: `proposed`, `accepted`, `superseded by ADR-XXXX`.
- Keep `README.md`'s documentation map in sync when adding docs.
