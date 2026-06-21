# API surface

Atlas exposes three API groups from the gateway. Compatibility surfaces are defined by _what real clients need_, not by cloning every provider endpoint.

## 1. Anthropic-compatible API (first-class — ADR-0002)

Target clients: Anthropic SDKs (Python/TS/Go/…), Claude Agent SDK, Claude Code. The bar: **an app using the Anthropic SDK works against Atlas by changing `ANTHROPIC_BASE_URL` (+ key + model name) and nothing else.**

### Endpoints

| Endpoint                                | v1 scope                                                                                                                                                                                                                                                                                                                                                                                                         |
| --------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /v1/messages`                     | Full support: system prompts, multi-turn, multimodal content blocks (images, model-permitting), **tool use** (`tools`, `tool_choice`, `tool_use`/`tool_result` blocks), **streaming** (SSE event sequence below), `stop_sequences`, `max_tokens`, `temperature`/`top_p`/`top_k` (passed to engine where supported; an omitted `temperature`/`top_p` picks up the model's catalog sampling default — M2 phase 4a) |
| `POST /v1/messages/count_tokens`        | Supported — agents use this for budgeting; counts via the target model's actual tokenizer                                                                                                                                                                                                                                                                                                                        |
| `GET /v1/models`, `GET /v1/models/{id}` | Lists deployed models _and_ aliases, Anthropic response shape                                                                                                                                                                                                                                                                                                                                                    |

### Streaming

SSE must emit the exact Anthropic event sequence — `message_start` → `content_block_start` → `content_block_delta` (`text_delta` / `input_json_delta`) → `content_block_stop` → `message_delta` (with `stop_reason`, `usage`) → `message_stop` — including correct `stop_reason` values (`end_turn`, `max_tokens`, `stop_sequence`, `tool_use`). Tool-use streaming (`input_json_delta`) is required: agent SDKs depend on it.

### Behavior notes

- **Auth:** accept the key from `x-api-key` _or_ `Authorization: Bearer` (clients vary); `anthropic-version` header accepted and ignored.
- **Model aliases:** operator-defined mapping, e.g. `claude-sonnet-4-6 → qwen3-coder-72b`, so SDK/tool defaults resolve. Claude Code users can alternatively use `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` env vars — document both.
- **Session affinity (optional, ADR-0011):** an `x-atlas-session` request header pins a conversation to a routing key, so multi-turn agents stick to the replica holding their warm prefix cache. It is a pure Atlas extension — never required, ignored when affinity is off — and absent it Atlas derives the key from the conversation's stable prefix, so the drop-in surface is unchanged. Affinity is a load-bounded hint: under load a conversation still spreads or sheds per the backpressure codes above.
- **Errors:** Anthropic error envelope (`{"type":"error","error":{"type":...,"message":...}}`) with matching status codes (400/401/404/413/429/500/529) so SDK retry logic behaves. Backpressure (ADR-0010) sheds two retryable codes, both carrying `Retry-After`: **429 `rate_limit_error`** (the model has live capacity but is momentarily full — admission queue full or max wait exceeded) and **529 `overloaded_error`** (no live replica can serve it right now). Both are mirrored on the OpenAI surface; **404 `not_found_error`** stays distinct (unknown/undeployed model).
- **Usage:** populate `usage.input_tokens`/`output_tokens` from engine counts (SDKs and cost dashboards read these).
- **Thinking blocks (ADR-0005):** `thinking` is accepted; reasoning output from reasoning-capable models (DeepSeek-R1, Qwen3, QwQ, GLM, gpt-oss, …) is mapped to Anthropic `thinking` content blocks incl. `thinking_delta` streaming. Non-reasoning models succeed with no thinking blocks. `thinking.type` accepts `enabled`, `adaptive` (Claude Code's default — the model decides whether to reason; treated as thinking-allowed), and `disabled`. `budget_tokens` is advisory (mapped to engine reasoning-effort where available, else ignored — never enforced by truncation). `redacted_thinking`/signature semantics not emulated.
- **Explicitly out of scope (return clean 400/404, documented):** prompt-caching `cache_control` (accepted and ignored, never an error), batches, Files API, Managed Agents, server-side tools (web search etc.). Open models won't emulate provider-side features; pretending otherwise breaks trust. Revisit individually post-v1.

## 2. OpenAI-compatible API (compat — ships v1, cheap, huge ecosystem)

Target clients: OpenAI SDKs, LangChain, llama-index, Continue, Open WebUI, anything else.

| Endpoint                    | v1 scope                                                                                                                                                                                                                                                             |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /v1/chat/completions` | Shipped (M0): system prompts, multi-turn, streaming, `tools`/`tool_calls` (request → `tool_calls` → tool result → answer), `stop`, `max_tokens`/`max_completion_tokens`, `temperature`/`top_p`, `finish_reason` mapping (`stop`/`tool_calls`/`length`), usage fields |
| `POST /v1/embeddings`       | M3 (needs an `embedding`-class model deployed): OpenAI embeddings shape, served by wrapping the engine's embedding task — [ADR-0012](decisions/0012-embeddings-and-reranker-model-classes.md)                                                                        |
| `POST /v1/rerank`           | M3 (needs a `reranker`-class model deployed): native Atlas endpoint following the de-facto Cohere rerank shape (query + documents + top_n → ordered results); no OpenAI equivalent to mirror — [ADR-0012](decisions/0012-embeddings-and-reranker-model-classes.md)   |
| `GET /v1/models`            | OpenAI list shape (same path as Anthropic's — disambiguate by response shape, which matches both closely enough; verify at build time, else version by header)                                                                                                       |
| `POST /v1/completions`      | Legacy text completion — passthrough if engine supports, low priority                                                                                                                                                                                                |

Even though engines (vLLM/SGLang/llama.cpp/Ollama) natively speak OpenAI-compat, Atlas **owns** this surface rather than proxying the engine's endpoint (m0-build-plan build-time decision 1): the gateway translates the internal representation ⇄ OpenAI wire itself, so behavior is identical across engines and matches the Anthropic surface's semantics (same auth, model aliases, context-window assertion, error mapping). Reasoning output is not surfaced here — the OpenAI chat surface has no portable thinking toggle, so `thinking` lives only on the Anthropic surface. `finish_reason` maps the internal stop reason: `end_turn`/`stop_sequence` → `stop`, `tool_use` → `tool_calls`, `max_tokens` → `length`.

## 3. Atlas admin API (`/api/v1/...`, native)

What the CLI and web console use. Not a compatibility surface — design for us.

| Area      | Endpoints (sketch)                                                                       |
| --------- | ---------------------------------------------------------------------------------------- |
| Workers   | list/inspect workers, generate join tokens, drain/remove                                 |
| Models    | registry CRUD, `pull` (download to workers), deploy/scale/stop instances, catalog browse |
| Instances | list running instances, health, logs                                                     |
| Keys      | create/revoke API keys, set allowed models                                               |
| Usage     | tokens by key/model/worker/time window                                                   |
| System    | health, version, license info                                                            |

Admin auth reuses the same API-key system: the `/admin/*` surface requires a key carrying the `admin` scope, not a separate token ([ADR-0008](decisions/0008-control-plane-persistence-and-api-keys.md)). Scoped keys and the `/admin/*` gate land in phase 5 (5a mints admin-scoped keys; 5b enforces the gate).

## Translation layer

One internal request/response representation; adapters at the edges:

```text
anthropic wire ⇄ ┐                          ┌ ⇄ engine A (OpenAI-compat endpoint)
                 ├─ internal representation ─┤
openai wire    ⇄ ┘                          └ ⇄ engine B (native API)
```

The hard parts, known in advance (LiteLLM's translation code is the reference for edge cases):

- **Tool calls:** Anthropic `tool_use` blocks ⇄ OpenAI `tool_calls`; `input_json_delta` streaming ⇄ OpenAI tool-call argument deltas; tool results as user-turn `tool_result` blocks ⇄ `role:"tool"` messages.
- **Content blocks vs string content**, system prompt as field vs message.
- **Stop reason vocabulary mapping** (`end_turn` ⇄ `stop`, `tool_use` ⇄ `tool_calls`, etc.).
- **Chat templates are the engine's job** — Atlas passes structured messages to engines, never renders prompts itself. The registry stores per-model template/parser _configuration_ the engine needs.

## Conformance testing (a deliverable, not an afterthought)

A test suite that runs the **real Anthropic and OpenAI SDKs** against Atlas covering: non-streaming + streaming text, multi-turn, tool loop (request → `tool_use` → `tool_result` → final), thinking blocks, count_tokens, model listing, error mapping, and Claude Code smoke test (`ANTHROPIC_BASE_URL` pointed at Atlas, scripted task). This suite is also marketing: a published compat matrix. Full spec: [conformance-suite.md](conformance-suite.md).
