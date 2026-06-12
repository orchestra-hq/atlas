# API surface

Atlas exposes three API groups from the gateway. Compatibility surfaces are defined by *what real clients need*, not by cloning every provider endpoint.

## 1. Anthropic-compatible API (first-class — ADR-0002)

Target clients: Anthropic SDKs (Python/TS/Go/…), Claude Agent SDK, Claude Code. The bar: **an app using the Anthropic SDK works against Atlas by changing `ANTHROPIC_BASE_URL` (+ key + model name) and nothing else.**

### Endpoints

| Endpoint | v1 scope |
|---|---|
| `POST /v1/messages` | Full support: system prompts, multi-turn, multimodal content blocks (images, model-permitting), **tool use** (`tools`, `tool_choice`, `tool_use`/`tool_result` blocks), **streaming** (SSE event sequence below), `stop_sequences`, `max_tokens`, `temperature`/`top_p`/`top_k` (passed to engine where supported) |
| `POST /v1/messages/count_tokens` | Supported — agents use this for budgeting; counts via the target model's actual tokenizer |
| `GET /v1/models`, `GET /v1/models/{id}` | Lists deployed models *and* aliases, Anthropic response shape |

### Streaming

SSE must emit the exact Anthropic event sequence — `message_start` → `content_block_start` → `content_block_delta` (`text_delta` / `input_json_delta`) → `content_block_stop` → `message_delta` (with `stop_reason`, `usage`) → `message_stop` — including correct `stop_reason` values (`end_turn`, `max_tokens`, `stop_sequence`, `tool_use`). Tool-use streaming (`input_json_delta`) is required: agent SDKs depend on it.

### Behavior notes

- **Auth:** accept the key from `x-api-key` *or* `Authorization: Bearer` (clients vary); `anthropic-version` header accepted and ignored.
- **Model aliases:** operator-defined mapping, e.g. `claude-sonnet-4-6 → qwen3-coder-72b`, so SDK/tool defaults resolve. Claude Code users can alternatively use `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` env vars — document both.
- **Errors:** Anthropic error envelope (`{"type":"error","error":{"type":...,"message":...}}`) with matching status codes (400/401/404/413/429/500/529) so SDK retry logic behaves.
- **Usage:** populate `usage.input_tokens`/`output_tokens` from engine counts (SDKs and cost dashboards read these).
- **Explicitly out of scope (return clean 400/404, documented):** extended/adaptive thinking semantics, prompt-caching `cache_control` (accepted and ignored, never an error), batches, Files API, Managed Agents, server-side tools (web search etc.). Open models won't emulate provider-side features; pretending otherwise breaks trust. Revisit individually post-v1 (a thinking-tokens mapping for reasoning models like DeepSeek-R1/QwQ is a plausible M3 feature).

## 2. OpenAI-compatible API (compat — ships v1, cheap, huge ecosystem)

Target clients: OpenAI SDKs, LangChain, llama-index, Continue, Open WebUI, anything else.

| Endpoint | v1 scope |
|---|---|
| `POST /v1/chat/completions` | Full: streaming, `tools`/`tool_calls`, JSON mode where engine supports |
| `POST /v1/embeddings` | Supported when an embedding model is deployed |
| `GET /v1/models` | OpenAI list shape (same path as Anthropic's — disambiguate by response shape, which matches both closely enough; verify at build time, else version by header) |
| `POST /v1/completions` | Legacy text completion — passthrough if engine supports, low priority |

Note: engines (vLLM/SGLang/llama.cpp/Ollama) all natively speak OpenAI-compat, so much of this group is proxy + auth + metering rather than translation.

## 3. Atlas admin API (`/api/v1/...`, native)

What the CLI and web console use. Not a compatibility surface — design for us.

| Area | Endpoints (sketch) |
|---|---|
| Workers | list/inspect workers, generate join tokens, drain/remove |
| Models | registry CRUD, `pull` (download to workers), deploy/scale/stop instances, catalog browse |
| Instances | list running instances, health, logs |
| Keys | create/revoke API keys, set allowed models |
| Usage | tokens by key/model/worker/time window |
| System | health, version, license info |

Admin auth is separate from inference API keys (admin tokens / console session).

## Translation layer

One internal request/response representation; adapters at the edges:

```
anthropic wire ⇄ ┐                          ┌ ⇄ engine A (OpenAI-compat endpoint)
                 ├─ internal representation ─┤
openai wire    ⇄ ┘                          └ ⇄ engine B (native API)
```

The hard parts, known in advance (LiteLLM's translation code is the reference for edge cases):

- **Tool calls:** Anthropic `tool_use` blocks ⇄ OpenAI `tool_calls`; `input_json_delta` streaming ⇄ OpenAI tool-call argument deltas; tool results as user-turn `tool_result` blocks ⇄ `role:"tool"` messages.
- **Content blocks vs string content**, system prompt as field vs message.
- **Stop reason vocabulary mapping** (`end_turn` ⇄ `stop`, `tool_use` ⇄ `tool_calls`, etc.).
- **Chat templates are the engine's job** — Atlas passes structured messages to engines, never renders prompts itself. The registry stores per-model template/parser *configuration* the engine needs.

## Conformance testing (a deliverable, not an afterthought)

A test suite that runs the **real Anthropic and OpenAI SDKs** against Atlas covering: non-streaming + streaming text, multi-turn, tool loop (request → `tool_use` → `tool_result` → final), count_tokens, model listing, error mapping, and Claude Code smoke test (`ANTHROPIC_BASE_URL` pointed at Atlas, scripted task). This suite is also marketing: a published compat matrix.
