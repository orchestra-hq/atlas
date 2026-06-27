---
title: API compatibility
description: The Anthropic-compatible and OpenAI-compatible APIs Atlas exposes, and what's out of scope.
---

Atlas exposes three API groups from the gateway. Compatibility surfaces are defined by _what real
clients need_, not by cloning every provider endpoint.

## Anthropic-compatible API (first-class)

Target clients: Anthropic SDKs (Python/TS/Go/…), the Claude Agent SDK, Claude Code. The bar: **an app
using the Anthropic SDK works against Atlas by changing `ANTHROPIC_BASE_URL` (+ key + model name) and
nothing else.**

| Endpoint                                | Scope                                                                                                                                                  |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /v1/messages`                     | System prompts, multi-turn, multimodal blocks (model-permitting), **tool use**, **streaming** (full SSE sequence), `stop_sequences`, sampling controls |
| `POST /v1/messages/count_tokens`        | Token budgeting via the target model's actual tokenizer                                                                                                |
| `GET /v1/models`, `GET /v1/models/{id}` | Lists deployed models _and_ aliases, Anthropic response shape                                                                                          |

### Streaming

SSE emits the exact Anthropic event sequence — `message_start` → `content_block_start` →
`content_block_delta` (`text_delta` / `input_json_delta`) → `content_block_stop` → `message_delta`
(with `stop_reason`, `usage`) → `message_stop` — with correct `stop_reason` values (`end_turn`,
`max_tokens`, `stop_sequence`, `tool_use`). Tool-use streaming (`input_json_delta`) is required —
agent SDKs depend on it.

### Behavior notes

- **Auth:** the key is accepted from `x-api-key` _or_ `Authorization: Bearer`; `anthropic-version` is
  accepted and ignored.
- **Model aliases:** operator-defined, e.g. `claude-sonnet-4-6 → qwen3-coder-72b`. Claude Code can
  also use `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`.
- **Thinking blocks:** `thinking` is accepted; reasoning output is mapped to Anthropic `thinking`
  blocks (incl. `thinking_delta` streaming). `thinking.type` accepts `enabled`, `adaptive` (Claude
  Code's default), and `disabled`. `budget_tokens` is advisory.
- **Session affinity (optional):** an `x-atlas-session` header pins a conversation to a routing key so
  multi-turn agents stick to a warm replica. Pure extension — never required, drop-in surface
  unchanged.
- **Errors:** the Anthropic error envelope with matching status codes. Backpressure sheds two
  retryable codes (both carry `Retry-After`): **429 `rate_limit_error`** (momentarily full) and **529
  `overloaded_error`** (no live replica right now). **404 `not_found_error`** stays distinct
  (unknown/undeployed model).
- **Served-by label:** every chat response carries `x-atlas-served-by` (`local`, or `cloud` when
  cloud-fallback spilled to an upstream provider). Out-of-band: the body is unchanged, so the SDK
  parses it identically.

### Out of scope (clean 400/404, documented)

Prompt-caching `cache_control` (accepted and ignored, never an error), batches, the Files API, Managed
Agents, and server-side tools (web search, etc.). Open models won't emulate provider-side features;
pretending otherwise breaks trust.

## OpenAI-compatible API

Target clients: OpenAI SDKs, LangChain, llama-index, Continue, Open WebUI, and more.

| Endpoint                    | Scope                                                                                                       |
| --------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `POST /v1/chat/completions` | System prompts, multi-turn, streaming, `tools`/`tool_calls`, `stop`, `max_tokens`, sampling, `finish_reason` |
| `POST /v1/embeddings`       | OpenAI embeddings shape; routes only to an `embedding`-class model                                          |
| `POST /v1/rerank`           | De-facto Cohere rerank shape; routes only to a `reranker`-class model                                       |
| `GET /v1/models`            | OpenAI list shape                                                                                           |
| `POST /v1/completions`      | Legacy text completion — passthrough where the engine supports it                                           |

Atlas **owns** this surface rather than proxying the engine's endpoint, so behavior is identical
across engines and matches the Anthropic surface's semantics. Reasoning output is not surfaced here —
the OpenAI chat surface has no portable thinking toggle. `finish_reason` maps the internal stop
reason: `end_turn`/`stop_sequence` → `stop`, `tool_use` → `tool_calls`, `max_tokens` → `length`.

## Atlas admin API

The native surface the CLI (and, later, the web console) use — workers, models, instances, keys,
usage, and the audit log. The `/admin/*` surface requires a key carrying the `admin` scope, reusing
the same API-key system rather than a separate token. See the [CLI reference](/atlas/reference/cli/).
