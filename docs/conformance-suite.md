# Conformance suite v0

The executable definition of M0's API surface. Each acceptance criterion in [m0-acceptance.md](m0-acceptance.md) maps to a test group here; the suite is the M0 release gate (breaking `ANTHROPIC_BASE_URL` drop-in blocks any release — roadmap standing track), and its published results are the compat matrix that proves positioning angle #1.

## Principles

1. **Real SDKs, not hand-rolled clients.** The product promise is "your existing client works", so the clients under test are the Anthropic Python and TypeScript SDKs, the OpenAI Python SDK, the Claude Agent SDK, and Claude Code itself.
2. **Two assertion layers.** SDK-level tests prove clients behave end-to-end; a wire-level layer captures raw SSE bytes and asserts exact event sequences, because SDKs normalize away details (event order, ping events, delta boundaries) that other clients depend on.
3. **Black-box.** The suite talks to a running Atlas endpoint and knows nothing about engines. The same tests run unchanged against every engine in the matrix.
4. **Structural assertions, not content.** Tasks are designed so any competent model passes (e.g. the answer is only obtainable by calling the provided tool); assertions check shapes, sequences, stop reasons, and JSON validity — never exact text. Sampling pinned to `temperature: 0`. One retry allowed per test; retries are recorded and reported as flakes, not hidden.
5. **Bidirectional mapping to acceptance criteria.** Every test cites its criterion; every criterion has at least one test. No orphan tests, no untested promises.

## Harness

```text
runner ──> atlas up (single-node, fixture config: tier aliases → catalog models)
   │
   ├─ pytest        : Anthropic Python SDK + OpenAI SDK + wire-level SSE capture
   ├─ vitest        : Anthropic TS SDK (streaming + tool loop subset)
   ├─ agent-sdk     : streamed agent loop, ≥3 client-side tool calls (G9)
   └─ claude-code   : real `claude` binary smoke via ANTHROPIC_BASE_URL (G9, capable tier)
   │
   └──> JUnit + matrix.json ──> published compat matrix (CI artifact)
```

Fixture config deploys two models per engine — one reasoning-capable, one non-reasoning — and maps the three tier aliases onto them.

The harness implementation lives in [/conformance](../conformance/README.md) (runner usage, `matrix.json` schema, current status). Built in m0-build-plan phase 1 — before the product — it first ran against a deliberately partial stub gateway so the matrix showed real structured results from day one. From phase 2 on, CI runs it against the real gateway (`atlas up` with a tiny llama.cpp model on a CPU runner); the stub stays as the default no-model local target. CI's `--require` gate widens as phases land.

### Matrix

| Dimension | Values                                                        |
| --------- | ------------------------------------------------------------- |
| Engine    | llama.cpp, vLLM (per M0; grows with adapters)                 |
| Model     | reasoning-capable, non-reasoning (from starter catalog)       |
| Client    | anthropic-py, anthropic-ts, openai-py, agent-sdk, claude-code |

Not a full cross product: wire-level and Python groups run everywhere; TS runs the streaming/tool subset on one engine; agent-sdk and claude-code runs execute once per engine against the sonnet alias.

## Test groups

### G1 — Messages basics (criterion 1 substrate)

Non-streaming single turn; system prompt honored; multi-turn; `stop_sequences` triggers `stop_sequence` stop reason; `max_tokens` triggers `max_tokens`; sampling params accepted; `usage` populated with non-zero input/output tokens.

### G2 — Streaming wire conformance (criterion 2)

Raw SSE capture asserting the exact sequence `message_start → content_block_start → content_block_delta* → content_block_stop → message_delta (stop_reason, usage) → message_stop`; `ping` events tolerated anywhere; `text_delta` concatenation equals final text; every stop-reason transition (`end_turn`, `max_tokens`, `stop_sequence`, `tool_use`) observed via dedicated cases; both SDKs stream the same content without error.

### G3 — Tool loop (criterion 3)

`tool_choice: auto/any/{specific tool}` each honored; `tool_use` block carries schema-valid JSON input; `input_json_delta` fragments concatenate to valid JSON; full round-trip (request → `tool_use` → `tool_result` → final answer); parallel tool calls in one assistant turn; `tool_result` with `is_error: true` handled; stop reason `tool_use` set whenever tools are called.

### G4 — Thinking (criterion 9, ADR-0005)

Reasoning model: `thinking` enabled yields `thinking` block(s) before text, streamed as `thinking_delta`; thinking blocks echoed back in multi-turn input are accepted; `budget_tokens` accepted (advisory). Non-reasoning model: same request succeeds with no thinking blocks and no error.

### G5 — Models and aliases (criterion 4)

`/v1/models` lists tier aliases and real models with context-window metadata; `claude-{opus,sonnet,haiku}-*` resolve to their configured models; `GET /v1/models/{id}` works for alias and real id.

### G6 — count_tokens (criterion 5)

Counts come from the target model's real tokenizer; alias and real model name agree; count matches `usage.input_tokens` of an identical request.

### G7 — Errors (criterion 6)

Oversized context rejected pre-dispatch with Anthropic-shaped 400 (gateway assertion — see [m0-acceptance.md](m0-acceptance.md) context-window handling); unknown model 404; bad/missing key 401; engine down 529; malformed body 400. Every envelope is `{"type":"error","error":{...}}`, and each SDK raises its corresponding typed exception (e.g. `BadRequestError`, `AuthenticationError`) — retry behavior on 529 observed, no retry on 400.

### G8 — OpenAI surface (criterion 7)

OpenAI SDK completes the G3 task against `/v1/chat/completions` with streaming + tools; `finish_reason` mapping (`stop`, `tool_calls`, `length`); usage fields populated.

### G9 — Agent harness end-to-end (criterion 1)

Two real-client cells exercise the agent loop through Atlas:

- **agent-sdk** (per-PR, CPU tier): a streamed agent loop completes ≥3 client-side tool calls — request → `tool_use` → client executes → `tool_result` → repeat — driven on the small catalog model. The tool is forced each turn (`tool_choice`), so the loop is deterministic: what is under test is Atlas's streamed multi-turn tool wire path, not the model's planning.
- **claude-code** (capable tier, opt-in via `CONF_CLAUDE_CODE_SMOKE`): the real `claude` binary, `ANTHROPIC_BASE_URL` pointed at Atlas, runs a non-interactive edit-and-verify task in a sandbox and exits successfully. This is the literal drop-in promise. It is off by default because the small CPU-tier model drives Claude Code only intermittently; reliable Claude Code drop-in — and the dedicated **Claude Agent SDK** package's model-initiated custom-tool loop — need a capable model and run in the full-matrix/GPU acceptance tier (see [open-questions.md](open-questions.md)). The smoke earns its keep: it caught Atlas rejecting Claude Code's default `thinking.type: "adaptive"` (now fixed).

### G10 — Ops minimum (criterion 8)

`/healthz` and `/readyz` semantics (ready only after a model is servable); token counts appear in logs for each request.

## Pass policy

M0 ships when every group is green in every defined matrix cell on both engines. Flake rate is published alongside the matrix. The suite versions with the API surface: a change that breaks a green test is a breaking change and needs an ADR or a fix.

## Out of scope for v0

Multimodal pass-through (not M0 acceptance), `/v1/embeddings`, legacy `/v1/completions`, load/latency benchmarks (M1+), multi-node routing (M1).
