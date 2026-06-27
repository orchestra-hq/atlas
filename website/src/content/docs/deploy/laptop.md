---
title: Laptop or dev box
description: Run an open model locally with llama.cpp — no GPU required.
sidebar:
  order: 1
---

The local path: run an open model on your own machine (no GPU needed) and drive Claude Code or an
OpenAI client against it. Great for development, evals, and offline work.

```sh
atlas up --model qwen3-0.6b
# in another shell:
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<printed key> claude
```

`atlas up` provisions the llama.cpp runtime on first use and serves on `http://localhost:8080`. The
default API key is printed on first start.

One-shot, no daemon:

```sh
atlas run qwen3-0.6b "your prompt"
```

Prefer containers? The [slim image](/atlas/deploy/docker/) does the same.

:::note
A small model on a laptop drives Claude Code only intermittently. For reliable agentic use, serve a
capable model on a [GPU box](/atlas/deploy/single-gpu-box/).
:::

See the [quickstart](/atlas/get-started/quickstart/) for the full first-run walkthrough.
