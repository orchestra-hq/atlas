---
title: Models & the catalog
description: The starter catalog, model classes, aliases, and serving your own weights.
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

## Serving your own weights

Beyond the catalog, `atlas up --model <ref>` serves a model by Hugging Face repo id or a local file
path (and an `https://` GGUF URL can be added as a catalog entry). Catalog models are the recommended
path because their templates and parsers are agent-tested; raw weights are best-effort for
template-sensitive behavior (notably reasoning toggles on hybrid models).

See [API compatibility](/atlas/reference/api-compatibility/) for endpoint details and the
[CLI reference](/atlas/reference/cli/) for `atlas pull` (fetch weights) and `atlas deploy` (place a
catalog model on the fleet).
