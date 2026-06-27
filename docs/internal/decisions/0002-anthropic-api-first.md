# ADR-0002: Anthropic Messages API is the first-class surface; OpenAI-compat ships alongside

**Status:** accepted (decision point 3 amended by ADR-0005: thinking blocks moved in scope)

## Context

The founding use case is pointing agents built on the Claude Agent SDK / Claude Code at Atlas via `ANTHROPIC_BASE_URL`. Meanwhile the broader ecosystem (LangChain, most tools) speaks the OpenAI API. Engines natively expose OpenAI-compat endpoints; vLLM and Ollama now also expose Anthropic-compat endpoints, so both surfaces are proven feasible.

## Decision

1. The **Anthropic Messages API** (`/v1/messages`, `count_tokens`, `/v1/models`) is the primary, conformance-tested, never-break surface. Drop-in Claude Agent SDK / Claude Code compatibility is a release gate.
2. The **OpenAI chat completions API** ships in v1 as a compatibility surface (it's cheap — engines speak it natively — and it unlocks the larger ecosystem).
3. Provider-side features open models can't honor (prompt caching semantics, batches, files, managed agents, server-side tools) are explicitly out of scope and documented as such, rather than half-emulated. `cache_control` is accepted and ignored (never an error). Thinking was originally on this list; ADR-0005 moved it in scope because open reasoning models genuinely produce it. See [api-surface.md](../../api-surface.md).

## Consequences

- Two wire formats must be maintained against one internal representation; the tool-calling translation is the hardest part and gets the most tests.
- "Agent-first / Anthropic-native" becomes a positioning asset no major orchestration competitor currently claims.
- We must track Anthropic API evolution (new content block types, event types) as ongoing maintenance.
