# Conformance suite v0

The executable definition of M0's API surface. Each acceptance criterion in [m0-acceptance.md](m0-acceptance.md) maps to a test group here; the suite is the M0 release gate (breaking `ANTHROPIC_BASE_URL` drop-in blocks any release — roadmap standing track), and its published results are the compat matrix that proves positioning angle #1.

> **Status:** the suite has grown with the project — groups G1–G22 (M0 through M3) plus G23 (M8 bring-any-model auto-config) are all implemented and run green per the milestone acceptance reports. Sections below labelled by milestone describe when each group was introduced, not work that is still outstanding.

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

The harness implementation lives in [/conformance](../../conformance/README.md) (runner usage, `matrix.json` schema, current status). Built in m0-build-plan phase 1 — before the product — it first ran against a deliberately partial stub gateway so the matrix showed real structured results from day one. From phase 2 on, CI runs it against the real gateway (`atlas up` with a tiny llama.cpp model on a CPU runner); the stub stays as the default no-model local target. CI's `--require` gate widens as phases land.

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

Multimodal pass-through (not M0 acceptance), `/v1/embeddings`, legacy `/v1/completions`, load/latency benchmarks (M1+). Multi-node routing is covered by G11 (M1, below).

---

## M1 test groups

These extend the matrix and the pass policy when M1 ships. See [m1-build-plan.md](m1-build-plan.md) for the phase that introduces each group.

### G11 — Multi-worker routing (M1 phase 4)

Two workers registered, each running a different model. Requests for model A route to worker A; requests for model B route to worker B. G1–G8+G10 pass against both workers independently. A request for an undeployed model returns a well-formed 404 (`not_found_error`).

### G12 — Auth (M1 phase 5)

Missing `x-api-key` → 401 Anthropic error envelope; invalid key → 401; valid key → request succeeds. Key with model allowlist: allowed model succeeds, disallowed model → 403. Key revocation takes effect immediately (no cache window).

### G13 — Usage metering (M1 phase 6)

After N requests across two keys and two models: `atlas usage` returns correct per-key, per-model, and per-worker totals; input + output token sums match the `usage` fields returned on individual responses; records survive process restart (durable in SQLite). Interrupted-stream case (review finding): a stream cut off partway (worker drop or client disconnect) still records the output tokens emitted up to the cut, rather than zero — an estimate when no exact engine count is available, since usage is reported only at end of stream.

### G14 — Fleet ops (M1 phase 7)

`wss://` connection works end-to-end (server with TLS cert, worker connecting over `wss://`). Env-var join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`) works without CLI flags. Worker drain: SIGTERM triggers drain, in-flight request completes, no new requests routed to draining worker. Heartbeat timeout: kill -9 on worker process; server marks it gone within 2× heartbeat interval.

---

## M2 test groups

These extend the matrix and the pass policy when M2 ships. See [m2-build-plan.md](m2-build-plan.md) for the phase that introduces each group.

### G15 — Observability (M2 phase 1)

`/metrics` exposes the core series (request rate/latency/error, per-model and per-worker token counters, in-flight gauges) and they move correctly across N requests and a worker drop. `atlas status` reports the same fleet state; `atlas usage --by-worker` attributes usage to stable worker names that survive a reconnect (not ephemeral per-connection ids). The admin CLI reaches a self-signed-TLS gateway with `--tls-pin`.

### G16 — Load balancing + backpressure (M2 phase 2)

Under concurrent load beyond the fleet's capacity for a model: requests spread across replicas by least-in-flight, excess requests queue up to the bound, and further requests shed with a well-formed retryable `429` (capacity momentarily full) or `529` (overloaded) carrying `Retry-After` — never a hang or a 5xx. Queue depth and shed counts appear in `/metrics`; usage records stay complete and correct under the load.

### G17 — Engine breadth (M2 phase 3)

G1–G8+G10 pass on MLX (Apple Silicon) and on SGLang (NVIDIA GPU), on the capable/nightly tier. The pinned engine version is the one provisioned; the upgrade flow moves a runtime to a new pinned version and the suite still passes.

### G18 — Catalog + capability matrix (M2 phase 4)

The agent-capability matrix is generated from real per-model×engine runs and reflects suite results. A request that omits sampling fields uses the model's catalog defaults (phase 4a); a reasoning model's thinking behavior follows its catalog config rather than the global convention (phase 4b).

The matrix is produced by [`conformance/capability_matrix.py`](../../conformance/README.md) (phase 4c): it aggregates the per-run `matrix-<engine>.json` files into one published matrix — a row per (model, engine) with an **agent-readiness verdict** (`ready` / `partial` / `incomplete` / `unsupported`) turning on the agent-critical groups (G3 tool use, G9 the streamed multi-call agent loop), plus the per-group detail. The generator and its verdict logic are unit-tested on the CPU per-PR tier, and the per-PR conformance job runs it on the single llama.cpp cell it produces; the **full** model×engine matrix is generated on the nightly capable tier, which needs the MLX (Apple-Silicon) and CUDA runners still dormant in [open-questions.md](open-questions.md) — the same blocker as G17.

---

## M3 test groups

These extend the matrix and the pass policy when M3 ships. See [m3-build-plan.md](m3-build-plan.md) for the phase that introduces each group.

### G19 — Prefix/session-affinity routing (M3 phase 1)

A conversation sent repeatedly across turns lands on the same replica while that replica has capacity, so a prefix-caching engine reuses its warm cache (affinity hit). Under load that pushes the affine replica past the configured tolerance, the request falls back to least-in-flight rather than queueing behind a busy replica — never a hang or a 5xx, and the G16 backpressure semantics still hold. Affinity hit/miss and per-replica warm-key counts appear in `/metrics` and `atlas top`.

### G20 — Embeddings + reranker model classes (M3 phase 2a–2b)

A deployed embedding model serves `POST /v1/embeddings` with correct-dimension vectors for the OpenAI SDK drop-in; a deployed reranker serves `POST /v1/rerank` and orders documents by relevance. A request sent to the wrong class (embeddings against a chat model, or vice versa) returns a clean, well-formed error, not a 5xx. Class-aware scheduling places embedding/reranker models on capable workers by the same VRAM-fit policy as chat models.

### G21 — Audit log (M3 phase 3)

Every control-plane mutation — model deploy/scale/stop, worker drain/remove, and API key create/revoke — produces an audit record carrying the actor (the admin key id for an HTTP action, `cli`/`system` for local key management), the action, the target, a timestamp, and the result. Records are append-only (no API mutates or deletes them) and durable across a control-plane restart. `atlas audit` (and the `GET /admin/audit` read API) lists them and filters by actor, action, target, and time window. Runtime upgrade is a worker-local operation outside the control plane, so it is not captured.

### G22 — Cloud-fallback passthrough (M3 phase 4)

With fallback enabled for a model, load beyond local capacity that G16 would shed is instead served by the configured upstream provider, and the response is labeled `x-atlas-served-by: cloud` with its tokens attributed to the cloud ledger class — the response body itself is a normal Atlas response, so the SDK is unaffected. With fallback disabled (the default), the identical overflow sheds with the ADR-0010 429/529 envelope, unchanged. Usage reporting separates cloud-served from locally-served tokens.

---

## M8 test groups

This group extends the matrix and the pass policy when M8 ships (bring any model / auto-configuration, [ADR-0015](decisions/0015-bring-any-model-auto-configuration.md)). See [m8-build-plan.md](m8-build-plan.md) for the phase that introduces it.

### G23 — Bring-any-model auto-config (M8 phase 5)

`atlas up --model <hugging-face-repo>` for a **known-family** repo that is **not in the starter catalog** auto-configures the full serving plan from the model's own metadata (the `Auto-configured … family` resolution path — not the bare passthrough, and not the unknown-family plain-chat warning), and the resulting endpoint is **agent-grade**: it passes the agent-critical groups **G3** (tool loop) and **G9** (the streamed ≥3-call agent-SDK loop), plus **G1/G2** substrate and **G4** reasoning, with no catalog row written by anyone. Proven per-PR on a real single-node llama.cpp CPU deployment by [`scripts/conformance-m8.sh`](../../scripts/conformance-m8.sh) (the `Conformance (M8)` CI job), which boots the repo, asserts the auto-config signal, then drives the model-agnostic harness against it. GPU-engine breadth — the parser-flag families (`--tool-call-parser hermes`/`qwen25`/… on vLLM/SGLang) that auto-config sets but llama.cpp drives from the chat template, and auto-config on MLX (Apple Silicon) — is the standing nightly follow-on, not part of the per-PR gate (see [m8-acceptance.md](m8-acceptance.md)).
