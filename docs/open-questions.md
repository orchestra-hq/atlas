# Open questions

Decisions that need the project owner's call (or at least sign-off). When a question is resolved, the outcome is recorded in the doc (or ADR) that owns it and the question is deleted here — git history keeps the trail; this file only ever lists what is actually open.

## Hard enforcement of `tool_choice` forcing (phase 4)

Atlas translates Anthropic `tool_choice` correctly — `auto → auto`, `any → required`, `{type:tool,name} → {type:function,function:{name}}`, `none → none` — and forwards it to the engine ([api-surface.md](api-surface.md), `internal/engines/llamacpp`). But the pinned llama.cpp build (`b9611`) with the Qwen2.5 chat template treats forcing as **advisory**: it selects the named/appropriate tool when the prompt motivates a call, yet will emit plain text (no tool call) when the prompt runs contrary to the forced choice. So `any` / `{tool}` do not _guarantee_ a tool call the way the Anthropic API does.

True forcing would require the gateway to constrain decoding (e.g. supply a GBNF/JSON-schema grammar that mandates a tool call) rather than relying on the engine to honor `tool_choice` — which edges toward reimplementing engine behavior (ADR-0001). **Open:** do we (a) accept best-effort forcing in M0 and document it, (b) add gateway-side grammar enforcement, or (c) gate it on an engine-version bump once a build enforces `tool_choice:required`? Until decided, the G3 suite asserts tool selection on prompt-motivated tasks (the conformance philosophy) rather than forcing against a contrary prompt.

## Resolved

How workers get engine runtimes — settled: M0 ships managed runtimes only (downloaded prebuilt llama.cpp binaries, a `uv`-managed vLLM venv), with the container path arriving at M1 behind the same provisioner interface. Recorded in [m0-build-plan.md](m0-build-plan.md#engine-runtime-provisioning) and implemented for llama.cpp in phase 2.
