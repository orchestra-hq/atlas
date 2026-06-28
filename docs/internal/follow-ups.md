# Follow-ups

Deferred, non-blocking work surfaced by code reviews — items intentionally **not** done in the milestone that surfaced them, parked here so signing a milestone "code-complete" never buries a known refinement inside that milestone's build plan.

Scope rules:

- Correctness and security findings are fixed in-milestone, not deferred here. This file holds only **non-blocking refinements** (efficiency, altitude, operability, UX) and the design decisions a few of them need before they can be done.
- Each entry states what, why it was deferred, where in the code it lives, a suggested milestone, and the review that surfaced it. Items needing an owner decision before work can start are marked **Decision needed** (those are the ones that would otherwise go in [open-questions.md](open-questions.md); they live here so all review fallout sits in one place).
- Delete an entry when it ships — git history keeps the trail.

## TLS / transport security

### Self-signed cert cache ignores changed `--tls` hosts

**Suggested:** M7 (packaging & IaC — bites on real public deploys; deferred from M5 per [ADR-0014](decisions/0014-m5-rescoped-to-documentation.md)). **Surfaced:** post-phases-6–7 review.

`loadOrCreateSelfSigned` (`internal/cli/tls.go`) returns the cached `cert.pem`/`key.pem` whenever both exist, without checking the requested SAN hosts still match. A host/hostname change keeps serving a cert whose SANs omit the new host, so a non-pinning client fails hostname validation until the files are deleted by hand. Low impact while pinned workers (which skip the name check) are the primary client.

**Decision needed:** the obvious fix — regenerate on SAN mismatch — changes the pin and breaks already-distributed worker pins. Pick: warn-and-keep, error-and-instruct (tell the operator to delete the cached files), or regenerate-and-reprint the new pin.

### ACME mode does not reconcile `--tls-acme-domain` with `--addr`

**Suggested:** M7 (packaging & IaC — when ACME is exercised on a real public deployment; deferred from M5 per [ADR-0014](decisions/0014-m5-rescoped-to-documentation.md)). **Surfaced:** post-phases-6–7 review.

TLS-ALPN-01 validation requires the server reachable on `:443`, but nothing checks the listen port, so `atlas server --tls-acme-domain …` left on the default `:9090` silently never obtains a cert (every handshake fails fetching one). Only a banner note states the `:443` requirement today (`internal/cli/tls.go`, `acmeTLS`). A hard check is wrong — operators legitimately sit behind an LB/port-forward doing `:443 → :9090` — so the right move is a startup _warning_ when the listen port is not 443 and no proxy is declared.

## Engine adapters / dispatch hot path

### Per-request token-count probe on MLX & SGLang doubles prefill

**Suggested:** the MLX/SGLang capable-runner tier (the Apple-Silicon runner is still dormant; the CUDA runner now exists for the vLLM nightly but the SGLang cell is not yet wired up — see [open-questions.md](open-questions.md)) — not exercisable in CI until then. **Surfaced:** M2 milestone review.

`assertContextFits` (`internal/server/gateway.go`) runs on every inference request and, when the model's context window is known, calls `CountTokens` for a pre-dispatch fit check. For llama.cpp and vLLM that is a cheap `/tokenize` call, but MLX and SGLang expose no tokenizer endpoint, so their adapters implement `CountTokens` as a one-token `Execute` probe — a full prefill. So every request to an MLX/SGLang model pays a second prefill before dispatch (and in fleet mode an extra wire round-trip), roughly doubling prefill cost. Both adapters' doc comments already warn the probe is "not for the hot path." Efficiency only — counts are correct and requests still serve — so it is deferred, but it bites every request once those engines run in anger.

The fix needs a "counting is cheap" capability to reach the gateway: `model.Exec` is a `*worker.Worker` or `*remoteWorker`, never the raw adapter, so the bit must flow engine → worker → over the wire (`model_ready`) → route, exactly as `ContextWindow` already does. The gateway then skips the prompt-token pre-check for probe-only engines (still enforcing the cheap `max_tokens ≥ window` check) and lets the engine surface an oversized prompt at dispatch. Fold in the duplication while there: the identical `CountTokens` probe in `internal/engines/mlx/adapter.go` and `internal/engines/sglang/adapter.go` should move to the shared `openaichat.Client` both embed.

## Runtime provisioning lifecycle

### `runtime upgrade --prune` and concurrent provisioning lack a liveness/lock guard

**Suggested:** M2/M3 runtime hardening, alongside the multi-process fleet work. **Surfaced:** M2 milestone review.

Two related cross-process gaps in `internal/runtime/provision.go`:

- **Prune deletes in-use versions.** `Prune` (via `atlas runtime upgrade --prune`, `internal/cli/runtime.go`) removes every engine version except the newly pinned one, with no check for a process still serving an older version; an engine imports Python modules from that venv lazily, so pruning mid-serve can break a running process. The M2 review made the help text honest (prune only when nothing is serving an older version); the robust fix is a liveness check that skips versions a running engine is bound to.
- **`sweepStaging` can wipe a sibling's staging tree.** Provisioning takes no cross-process lock and runs from many entry points (`atlas up`, `run`, `models`, `runtime provision/upgrade`). `sweepStaging` `os.RemoveAll`s every `<engine>-stage-*` dir by shared prefix, so two concurrent provisions of the same engine can destroy each other's in-flight staging during `uv pip install`.

**Decision needed:** the durable fix is cross-process coordination — a per-engine lockfile (flock) around provisioning plus a PID/liveness record per provisioned version. Pick the mechanism (lockfile vs. a state file the worker writes) before implementing; both prune-safety and concurrent-provision-safety fall out of it.

## API surface / drop-in compatibility

### Cloud-fallback usage is sniffed from the relayed body, not parsed structurally

**Suggested:** when cloud-fallback sees real traffic, or alongside the base64-embeddings work. **Surfaced:** M3 phase 4 build.

`tokenSniffer` (`internal/server/cloud.go`) extracts cloud-spill usage by regex-matching the providers' token fields (`input_tokens`/`output_tokens`, `prompt_tokens`/`completion_tokens`) off the relayed byte stream, keeping the last value seen. This is deliberately format-agnostic (one path covers buffered + SSE, Anthropic + OpenAI) and robust for today's shapes, but it is not a structural parse: if a provider renamed a usage field or nested it differently, the sniff would silently record zero rather than error. The same zero-attribution happens on any upstream 2xx the regex doesn't match and on a client disconnect mid-stream (the usage event may never arrive), yet the spill is still recorded as "served". Cloud spend would then under-report — which matters because the point of the cloud ledger class is to monitor and cap real spend.

**Decision (2026-06-22):** best-effort. The fields are stable across both providers and the sniffer is covered by tests. Revisit only if a real provider response proves it insufficient — no code change — but tracked so an under-reporting bill is not a surprise.

### Wrong-class request to a cold model autostarts it instead of a clean 400

**Fixed (2026-06-22).** `Autostarter` now carries `ClassOf(model string) (class string, ok bool)`, implemented by the scheduler via a catalog lookup. `dispatchPrep` checks the declared class before calling `ensure` when no live replica is present, so a wrong-class cold request returns a 400 without triggering a wasted autostart.

### `/v1/embeddings` always returns float vectors, ignoring `encoding_format: base64`

**Fixed (2026-06-22).** `FromCoreEmbeddings` now accepts `encodingFormat` and base64-encodes little-endian IEEE 754 float32 bytes per vector when the client requests `"base64"` (the OpenAI Python SDK default), returning a JSON string instead of a float array. Float format (or empty) is unchanged. The handler passes `req.EncodingFormat` through. Covered by unit tests in `internal/api/openai/embeddings_test.go` and a handler-level test in `internal/server/embeddings_test.go`.

### `enable_thinking:false` dropped for hybrid models served outside the catalog

**Suggested:** the phase that owns reasoning config (phase 4b follow-up / catalog expansion). **Surfaced:** M2 milestone review.

Phase 4b made the `enable_thinking` kwarg catalog-driven: `ThinkingKwargs` (`internal/engines/openaichat/wire.go`) omits it for any model with `reasoning=false`. That re-opens a drop-in regression for a **hybrid** model (e.g. Qwen3) served via a raw `.gguf`/HF spec rather than a catalog entry — it is classified non-reasoning, so the kwarg is omitted and the model's chat template can default thinking _on_, emitting unrequested `<think>` blocks to clients that did not ask for them. The catalog path is correct; only the raw escape-hatch path regresses.

**Decision (2026-06-22):** document raw-served hybrids as best-effort — the catalog is required for thinking control. No code change. The `ThinkingKwargs` doc comment already notes this; no further action until a hybrid model enters the shipped catalog on the raw path.

### Cloud-fallback spill wired into two handlers by hand

**Suggested:** opportunistically, when next touching the dispatch surfaces. **Surfaced:** M3 review (all-of-M3).

The shed-vs-spill decision (`shouldSpill` + `spillToCloud`, `internal/server/cloud.go`) is wired into both `handleMessages` (`internal/server/gateway.go`) and `handleChatCompletions` (`internal/server/openai.go`) as near-identical blocks differing only in the unreachable-upstream error renderer (`overloadedErr` vs `writeOpenAIErr`). A change to the spill trigger set (e.g. adding a status) or to the relay must be made in both, and a miss means a model is covered by cloud-fallback on one API surface but sheds on the other. Fold the decision into one wrapper around `dispatchPrep`'s error path, parameterized by a surface-specific error writer. Altitude only — both copies are correct today.

## Conformance / engine behavior

### Budget-truncated reasoning on vLLM's qwen3 parser surfaces nothing

**Suggested:** when the vLLM/GPU tier is being iterated anyway (needs a GPU to repro). **Surfaced:** 2026-06-24, diagnosing the vLLM G4 failures ([open-questions.md](open-questions.md)).

When a Qwen3 thinking turn on vLLM hits `max_tokens` before emitting `</think>`, the response comes back with empty `content` _and_ empty `reasoning_content` despite a full `output_tokens` count — so neither Atlas nor any OpenAI client sees the generated text. This is why the G4 thinking tests failed at `max_tokens=128`; M0 now gates on a realistic budget instead (the model completes its trace), so this is no longer blocking. But it remains a real edge: vLLM 0.23.0 contains the upstream #35221 fix (which should route truncated output to `reasoning_content` when `enable_thinking=True`) and Atlas does send `chat_template_kwargs.enable_thinking=true` (`internal/engines/openaichat/wire.go`, `ThinkingKwargs`), yet the truncated trace still vanishes. Worth confirming on a GPU box whether the parser honors `enable_thinking` as passed (capture vLLM's raw `/v1/chat/completions` for a deliberately-truncated thinking request), and whether a short streamed budget reproduces it. Contrast: llama.cpp surfaces a partial `reasoning_content` in the same situation, so the two engines diverge on truncated reasoning.

## Documentation / research sourcing

### Research docs cite blog aggregators instead of primary sources

**Suggested:** opportunistically, when next touching the research docs. **Surfaced:** pre-go-live internal-docs review (2026-06-28).

Several load-bearing claims in `docs/internal/research/` cite third-party blog/SEO-aggregator pages rather than the primary project docs that repo rule 6 calls for. Specifically: `distribution-deployment-and-gpu-ci.md` sources some competitive distribution claims (e.g. vLLM Docker-first) to markaicode.com / spheron.network and leaves the competitor comparison table's per-row claims unlinked; `landscape.md` leaves the TensorRT-LLM and TGI rows uncited (the "TGI archived March 2026" date is a specific, dateable assertion with no source); `model-catalog-m0.md` cites comparison/aggregator sites (docsbot.ai, computertech.co, morphllm.com) for model license/context/benchmark claims. None are blocking — these are point-in-time research snapshots — but each load-bearing claim should be re-pointed at a primary source (project docs, HF model cards, official repos), keeping aggregators only for synthesis/cost commentary that has no primary equivalent. Doing it properly means re-verifying each link, so it is parked here rather than rushed.

## Minor refinements (M2 review)

Low-severity items from the M2 review, none blocking. **Suggested:** opportunistically, when next touching each area.

- **`routeCount` locked 2–3× per dispatched request** (`internal/server/gateway.go`): `ensure`, `Admission.Acquire`'s replica-count callback, and `pick` each take `g.mu` for the same logical decision. Threading the count read once through `dispatchPrep` would drop a couple of RWMutex round-trips from the hot path.
- **OpenAI handler special-cases `ErrNotFound`** (`internal/server/openai.go`): `dispatchPrep` speaks only the Anthropic envelope, so the OpenAI mirror re-maps not-found by hand. Folding the not-found code into `openaiErrType` would let every shed/error type translate uniformly with no per-type branch.
- **OpenAI handler preamble duplicated across three handlers** (`internal/server/openai.go`, M3): `handleChatCompletions`, `handleEmbeddings`, and `handleRerank` repeat the same auth → read → unmarshal → model-required → `modelPermitted` → `dispatchPrep` → not-found-mapping → capability-assert → usage-record sequence. A shared `prepareOpenAIDispatch(w, r, model, class)` helper would collapse the three copies (and is where the not-found folding above lands). Touches the permission/auth path, so do it deliberately with the three handlers' tests as the guard.
- **MLX served-id re-derived positionally** (`internal/worker/worker.go`): `engineSetup` recovers the served id as `ModelArgs[n-1]`, which `resolve.go` already knows; threading the resolved served name through `worker.Config` (as `ContextWindow`/`Reasoning` already are) removes the positional assumption.
- **`AsyncUsageWriter.Record` block-contract depends on the caller's deadline** (`internal/server/usagewriter.go`): the exported method's "blocks briefly" promise holds only because the sole caller passes a write timeout. The inline-persist fallback was bounded in the M2 review, but a future caller passing `context.Background()` on a full buffer would still block on enqueue; consider an internal enqueue deadline.
- **`Stop` deletes a deployment with waiters>0** (`internal/server/scheduler.go`): an `EnsureModel` waiter can be Stopped out from under. Verified harmless today (each waiter decrements its own captured pointer, so no counter corruption) but untested — a guard or a regression test would lock the behavior in.
