---
title: Models & the catalog
description: The starter catalog, model classes, aliases, and bringing any model with auto-configuration.
sidebar:
  order: 3
---

Atlas ships a **starter catalog** of agent-tested models with the correct chat templates, tool
parsers, and per-model sampling/reasoning configuration. `atlas up --model <name>` and
`atlas pull <name>` resolve names against it.

## Model classes

Models declare a `class`, and endpoints route only to a matching class (a mismatch is a clean 400):

| Class       | Endpoint                | Example catalog model    |
| ----------- | ----------------------- | ------------------------ |
| `chat`      | `/v1/messages`, `/v1/chat/completions` | `qwen3-8b`, `gemma-4-12b-coder` |
| `embedding` | `/v1/embeddings`        | `nomic-embed-text-v1.5`  |
| `reranker`  | `/v1/rerank`            | `bge-reranker-v2-m3`     |

## Reasoning models

The catalog includes reasoning-capable models (e.g. Qwen3, GLM) whose reasoning output is mapped to
Anthropic `thinking` blocks. Reasoning is controlled per-model via the catalog, not guessed — see
[Use with Claude Code](/atlas/guides/claude-code/).

## Aliases

A model can carry aliases so that SDK/tool defaults resolve to it. Operators can also map Claude model
names to local models (e.g. `claude-sonnet-4-6 → qwen3-coder-72b`) — see
[Use with Claude Code](/atlas/guides/claude-code/).

## Bring your own model

Beyond the catalog, `atlas up --model <hugging-face-repo>` serves any model by its Hugging Face repo
id (or a local file path). You don't write a catalog entry: Atlas fetches the model's metadata — not
its weights — decides whether it can serve the model well, and configures it from that metadata. The
outcome is one of three, decided **before** any weights download:

- **Known family → auto-configured and agent-grade.** When the model's family is one Atlas has tested
  agent-config for (the family map in its own source), `atlas up` derives the tool-call and reasoning
  parsers, sampling defaults, and context window from the model's metadata and serves it exactly as a
  catalog model would — tool calling and reasoning work out of the box. The starter catalog is the
  curated fast path, not a fence.
- **Loadable but unknown family → served as chat, with a warning.** When the engine can load the model
  and it fits, but Atlas has no tested agent-config for its family, it is served as plain chat with a
  one-line startup warning (agent behavior is best-effort) plus a pointer to the one-line change that
  adds support for the family. Pass `--require-verified` to refuse such a model instead of serving it
  best-effort.
- **Can't load or won't fit → a clean failure.** An architecture the pinned engine can't load, or a
  model too large for the host's memory, fails fast with the reason (an upstream-engine pointer, or the
  memory shortfall) — no half-broken serve, no wasted download.

Preview the verdict for any repo without downloading weights with `atlas inspect <model>`: it prints
the derived engine, context window, template, and parsers, and which of the three outcomes applies. For
a multi-quant GGUF repo, `--quant` selects the quantization (the default prefers `Q4_K_M`).

See [API compatibility](/atlas/reference/api-compatibility/) for endpoint details and the
[CLI reference](/atlas/reference/cli/) for `atlas inspect` (preview a model), `atlas pull` (fetch
weights), and `atlas deploy` (place a catalog model on the fleet).
