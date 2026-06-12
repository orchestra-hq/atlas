# M0 acceptance spec

Derived from the API usage of Will's existing app (questionnaire answered 2026-06-12) plus the M0 demo bar. This is the definition of done for M0's API surface.

## Source: the reference app's profile

| Question | Answer | Consequence for Atlas |
|---|---|---|
| Agent SDK harness or raw Messages API? | **Claude Agent SDK harness** today (could be refactored to raw Messages API). 100% reliant on streaming + client-side tool loop. | Streaming SSE and the full tool-use loop are the non-negotiable core. Conformance must run the *actual Agent SDK harness*, not just the Messages API. |
| Server-side tools? | No. Each runtime gets its own machine with restricted execution and tightly restricted network egress. | Nothing to emulate — our "honest API scope" stance holds. Bonus: the app's egress-restricted runtimes only need to reach one Atlas endpoint, which the zero-inbound/single-endpoint design suits well. |
| Model tiers / context length? | All three tiers (opus/sonnet/haiku); **any length of request**. | Alias mapping must cover all three tiers to differently-sized local models. ⚠️ See context-window flag below. |
| Multimodal? | Not now, possibly later. | Image content blocks: accept-and-pass-through where the model supports it, but not M0 acceptance criteria. |
| Concurrency? | Not deployed yet; must scale. | No hard number to size against. M0 is correctness; concurrency/replicas are M1/M2. Capture real numbers once the app deploys. |
| Batches / files / thinking? | Not currently. | Stay out of scope per ADR-0002. |

## ⚠️ Flag for Will: "any length of request" has a ceiling on open models

Claude models accept up to 1M tokens. Open models served by Atlas will typically offer **32k–256k** depending on the model (and VRAM: KV cache for long contexts is expensive — long-context serving may dictate bigger GPUs than the parameter count suggests). If the app genuinely sends very long requests, then either:

1. the app needs graceful handling of smaller context windows (it will get a clean Anthropic-style 400/`max_tokens`-style error from Atlas — we make sure the error is well-formed), and/or
2. the catalog needs designated long-context models for the opus alias, sized accordingly.

Atlas's job: report each model's real context window via `/v1/models` metadata and fail with correct, SDK-parseable errors — never silently truncate. The app-side strategy is Will's call when deployment nears.

## Acceptance criteria

All run against a single-node `atlas up` with a catalog model on (a) llama.cpp and (b) vLLM:

1. **Agent SDK harness end-to-end**: a Claude Agent SDK app (and Claude Code as proxy for it) pointed at Atlas via `ANTHROPIC_BASE_URL` completes a multi-step task involving ≥3 client-side tool calls, streamed.
2. **Streaming conformance**: exact Anthropic SSE event sequence incl. `input_json_delta` tool-arg streaming and correct `stop_reason` transitions, validated by the real Anthropic SDK (Python + TS).
3. **Tool loop conformance**: `tools` + `tool_choice` honored; `tool_use` → `tool_result` → final answer round-trip; parallel tool calls in one assistant turn handled.
4. **Tier aliases**: `claude-{opus,sonnet,haiku}-*` aliases resolve to three configured local models; `/v1/models` lists aliases and real models with context-window metadata.
5. **count_tokens** returns real tokenizer counts for the target model.
6. **Error conformance**: oversized context, unknown model, bad auth, and engine-down each produce Anthropic-shaped error envelopes the SDK classifies correctly (400/404/401/529).
7. **OpenAI surface**: same task completes via OpenAI SDK against `/v1/chat/completions` with streaming + tools.
8. **Ops minimum**: `/healthz`, `/readyz`, single-directory state, logged token counts.
