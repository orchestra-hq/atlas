# ADR-0005: Thinking blocks are supported, mapped from engine reasoning output

**Status:** accepted

## Context

ADR-0002 originally listed extended-thinking semantics as out of scope, alongside batches/files/server-side tools, on the assumption that open models can't honor provider-side features. Two facts undermine that for thinking specifically:

1. **Open reasoning models are mainstream.** DeepSeek-R1, Qwen3 (hybrid thinking), QwQ, GLM-4.x, gpt-oss, and Kimi K2 Thinking all emit explicit reasoning traces, and vLLM/SGLang ship reasoning parsers that separate reasoning content from the final answer. The raw material for honest thinking-block support exists — this is unlike batches or server-side tools, which have no open-model equivalent.
2. **Claude Code — the M0 demo harness — enables thinking by default** on newer models. A surface that rejects or mishandles `thinking` puts the headline demo at risk.

## Decision

1. The `thinking` request parameter is **accepted**, and engine reasoning output is **mapped to Anthropic `thinking` content blocks**, including `thinking_delta` streaming events in the SSE sequence.
2. **Non-reasoning models degrade gracefully:** a request with `thinking` enabled succeeds and returns a response without thinking blocks. Never an error.
3. **Thinking budgets are advisory.** `budget_tokens` is translated to the engine's nearest equivalent (e.g. reasoning effort) where one exists, otherwise ignored. Atlas never enforces budgets by truncating reasoning. This is documented plainly.
4. **Signature/redaction semantics are not emulated.** `redacted_thinking` and thinking-block signature verification are provider-side concepts; Atlas accepts thinking blocks echoed back in multi-turn input and passes or strips them per the model's template requirements.
5. **In scope from M0** (previously slated M3): minimal mapping for reasoning-capable catalog models, with conformance cases in suite v0.

## Consequences

- Amends ADR-0002 decision point 3: thinking moves from the out-of-scope list to supported. Batches, files, managed agents, prompt-caching semantics, and server-side tools remain out.
- The M0 starter catalog must include at least one reasoning-capable model.
- The translation layer and model registry gain per-model reasoning-parser configuration (which engine parser, which tags) alongside chat-template config. **Implemented incrementally:** the engine-launch parser flags (`--reasoning-parser`, `--tool-call-parser`) live in each catalog entry's `engine_args` (phase 9); M2 phase 4b made the chat-template `enable_thinking` kwarg consult the entry's `reasoning` capability, so the kwarg is emitted only for a reasoning model — a non-reasoning model omits it rather than relying on the convention being a harmless no-op (the flag is threaded `worker.Config` → `openaichat.Client`, gated in `ThinkingKwargs`). A richer per-model reasoning _style_ (distinct kwarg names, system-prompt tokens, always-on models) is still future work, tracked in [open-questions.md](../open-questions.md).
- The conformance suite gains thinking cases: thinking-block streaming with a reasoning model, graceful no-thinking response with a non-reasoning model, multi-turn echo of thinking blocks.
- "Honest API scope" positioning is unaffected: we support thinking where the model genuinely reasons, and say exactly what budgets do.
