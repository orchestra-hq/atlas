---
title: Use with OpenAI SDKs & LangChain
description: Point OpenAI SDKs, LangChain, and other OpenAI-compatible clients at Atlas.
sidebar:
  order: 2
---

Alongside the Anthropic surface, Atlas exposes an OpenAI-compatible API, so OpenAI SDKs, LangChain,
llama-index, Continue, Open WebUI, and similar clients work against the same endpoint.

## Point an OpenAI client at Atlas

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="<your atlas key>",
)

resp = client.chat.completions.create(
    model="qwen3-0.6b",
    messages=[{"role": "user", "content": "Hello from Atlas"}],
)
print(resp.choices[0].message.content)
```

## What works

- **`POST /v1/chat/completions`** — system prompts, multi-turn, streaming, `tools`/`tool_calls`,
  `stop`, `max_tokens`/`max_completion_tokens`, `temperature`/`top_p`, `finish_reason` mapping, and
  usage fields.
- **`POST /v1/embeddings`** — OpenAI embeddings shape, served by an `embedding`-class model.
- **`POST /v1/rerank`** — the de-facto Cohere rerank shape, served by a `reranker`-class model.
- **`GET /v1/models`** — OpenAI list shape.

Atlas **owns** this surface rather than proxying the engine's endpoint, so behavior is identical
across engines (vLLM, SGLang, llama.cpp, MLX) and matches the Anthropic surface's semantics — same
auth, aliases, context-window assertion, and error mapping.

:::note
Reasoning/thinking output is surfaced only on the Anthropic surface — the OpenAI chat API has no
portable thinking toggle. Use the [Anthropic surface](/atlas/guides/claude-code/) if you need
reasoning traces.
:::

See [API compatibility](/atlas/reference/api-compatibility/) for the full surface.
