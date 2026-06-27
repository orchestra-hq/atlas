---
title: Quickstart
description: Serve an open model with atlas up and drive Claude Code against it.
sidebar:
  order: 2
---

This is the hero path: run an open model on your own machine and drive Claude Code against it — no GPU
required.

## 1. Serve a model

```sh
atlas up --model qwen3-0.6b
```

`atlas up` pulls the model from the starter catalog, provisions the engine runtime on first use, and
serves on `http://localhost:8080`. On first start Atlas mints a default API key and prints it once —
copy it from the logs.

## 2. Point Claude Code at it

In another shell:

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<printed key> claude
```

That's it — Claude Code now talks to your local model. The same base URL works for the Anthropic SDKs
and the Claude Agent SDK; see [Use with Claude Code](/atlas/guides/claude-code/).

## One-shot, no daemon

To run a single prompt without keeping a server up:

```sh
atlas run qwen3-0.6b "Write a haiku about local inference."
```

## Next steps

- [Use with Claude Code](/atlas/guides/claude-code/) — model aliases and default-model env vars
- [Use with OpenAI SDKs & LangChain](/atlas/guides/openai-and-other-clients/)
- [API compatibility](/atlas/reference/api-compatibility/) — exactly what the endpoint exposes
- [Deploy](/atlas/deploy/) — move from a laptop to a GPU box or a fleet

:::note
A small model drives Claude Code only intermittently. For reliable agentic use, serve a capable model
on a GPU — see [Deploy](/atlas/deploy/).
:::
