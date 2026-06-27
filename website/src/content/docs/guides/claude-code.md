---
title: Use with Claude Code
description: Point Claude Code and the Anthropic SDKs at Atlas with ANTHROPIC_BASE_URL.
sidebar:
  order: 1
---

Atlas implements the Anthropic Messages API as a first-class surface, so an app using the Anthropic
SDK, the Claude Agent SDK, or Claude Code works against Atlas by changing `ANTHROPIC_BASE_URL` (plus
key and model name) and nothing else.

## Point Claude Code at Atlas

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<your key> claude
```

The key is the one Atlas printed on first start (or any you mint with `atlas keys create`). The same
two variables work for the Anthropic Python/TypeScript/Go SDKs and the Claude Agent SDK.

## Model names and aliases

Operators define aliases that map a requested model to a deployed one, so SDK and tool defaults
resolve. For example, an operator might map `claude-sonnet-4-6 → qwen3-coder-72b`. Two ways to control
which local model Claude Code uses:

- **Aliases** — request the Claude model name your tooling defaults to; the operator's alias resolves
  it to a local model.
- **Default-model env vars** — set `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`,
  or `ANTHROPIC_DEFAULT_HAIKU_MODEL` to a model Atlas serves.

## What works

- Streaming, multi-turn, system prompts, and **tool use** (the agent tool loop) — the full Anthropic
  SSE event sequence, including tool-call streaming that agent SDKs depend on.
- **Thinking blocks** for reasoning-capable models (DeepSeek-R1, Qwen3, QwQ, GLM, gpt-oss, …), mapped
  to Anthropic `thinking` content blocks. `thinking.type: adaptive` (Claude Code's default) is
  accepted. Non-reasoning models simply return no thinking blocks.
- `count_tokens` for budgeting, and `GET /v1/models` listing deployed models and aliases.

See [API compatibility](/atlas/reference/api-compatibility/) for the precise surface and what is
intentionally out of scope.
