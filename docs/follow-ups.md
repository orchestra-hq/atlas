# Follow-ups

Deferred, non-blocking work surfaced by code reviews — items intentionally **not** done in the milestone that surfaced them, parked here so signing a milestone "code-complete" never buries a known refinement inside that milestone's build plan.

Scope rules:

- Correctness and security findings are fixed in-milestone, not deferred here. This file holds only **non-blocking refinements** (efficiency, altitude, operability, UX) and the design decisions a few of them need before they can be done.
- Each entry states what, why it was deferred, where in the code it lives, a suggested milestone, and the review that surfaced it. Items needing an owner decision before work can start are marked **Decision needed** (those are the ones that would otherwise go in [open-questions.md](open-questions.md); they live here so all review fallout sits in one place).
- Delete an entry when it ships — git history keeps the trail.

## TLS / transport security

### Self-signed cert cache ignores changed `--tls` hosts

**Suggested:** M5 (packaging & deployment — bites on real public deploys). **Surfaced:** post-phases-6–7 review.

`loadOrCreateSelfSigned` (`internal/cli/tls.go`) returns the cached `cert.pem`/`key.pem` whenever both exist, without checking the requested SAN hosts still match. A host/hostname change keeps serving a cert whose SANs omit the new host, so a non-pinning client fails hostname validation until the files are deleted by hand. Low impact while pinned workers (which skip the name check) are the primary client.

**Decision needed:** the obvious fix — regenerate on SAN mismatch — changes the pin and breaks already-distributed worker pins. Pick: warn-and-keep, error-and-instruct (tell the operator to delete the cached files), or regenerate-and-reprint the new pin.

### ACME mode does not reconcile `--tls-acme-domain` with `--addr`

**Suggested:** M5 (packaging & deployment — when ACME is exercised on a real public deployment). **Surfaced:** post-phases-6–7 review.

TLS-ALPN-01 validation requires the server reachable on `:443`, but nothing checks the listen port, so `atlas server --tls-acme-domain …` left on the default `:9090` silently never obtains a cert (every handshake fails fetching one). Only a banner note states the `:443` requirement today (`internal/cli/tls.go`, `acmeTLS`). A hard check is wrong — operators legitimately sit behind an LB/port-forward doing `:443 → :9090` — so the right move is a startup _warning_ when the listen port is not 443 and no proxy is declared.

## Engine adapters / dispatch hot path

### Per-request token-count probe on MLX & SGLang doubles prefill

**Suggested:** the MLX/SGLang capable-runner tier (the dormant Apple-Silicon + CUDA runners in [open-questions.md](open-questions.md)) — not exercisable in CI until then. **Surfaced:** M2 milestone review.

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

`tokenSniffer` (`internal/server/cloud.go`) extracts cloud-spill usage by regex-matching the providers' token fields (`input_tokens`/`output_tokens`, `prompt_tokens`/`completion_tokens`) off the relayed byte stream, keeping the last value seen. This is deliberately format-agnostic (one path covers buffered + SSE, Anthropic + OpenAI) and robust for today's shapes, but it is not a structural parse: if a provider renamed a usage field or nested it differently, the sniff would silently record zero rather than error. Cloud spend would then under-report — which matters because the point of the cloud ledger class is to monitor and cap real spend.

**Decision needed:** acceptable as best-effort (the fields are stable across both providers and the sniffer is covered by tests), or worth a per-provider structural usage parse keyed on the request surface. Recommend leaving it best-effort until a real provider response proves it insufficient — no code change — but tracked so an under-reporting bill is not a surprise.

### `/v1/embeddings` always returns float vectors, ignoring `encoding_format: base64`

**Suggested:** M3 phase 2b (the embeddings/rerank phase) or the first real-engine G20 nightly run. **Surfaced:** M3 phase 2a build.

`handleEmbeddings` (`internal/server/openai.go`) and `openai.FromCoreEmbeddings` always emit `embedding` as a JSON float array, and `openaichat.Client.Embed` pins `encoding_format: float` to the engine. The OpenAI Python SDK, however, defaults `client.embeddings.create(...)` to `encoding_format: base64` and decodes base64 on the way back. A client that leaves the default (rather than passing `encoding_format="float"`) sends `base64` and may not parse a float array — a drop-in gap for the headline SDK. The per-PR tier (float) is unaffected; this surfaces on the real-SDK G20 run.

**Decision needed:** (a) honor `encoding_format` — base64-encode little-endian float32 bytes per vector when requested, changing the response `embedding` field to `string | []float32` — **recommended**, it is what true SDK drop-in requires; or (b) document float-only and require callers to pass `encoding_format="float"`. Touches the OpenAI drop-in promise, so resolve before declaring G20 green on the real tier.

### `enable_thinking:false` dropped for hybrid models served outside the catalog

**Suggested:** the phase that owns reasoning config (phase 4b follow-up / catalog expansion). **Surfaced:** M2 milestone review.

Phase 4b made the `enable_thinking` kwarg catalog-driven: `ThinkingKwargs` (`internal/engines/openaichat/wire.go`) omits it for any model with `reasoning=false`. That re-opens a drop-in regression for a **hybrid** model (e.g. Qwen3) served via a raw `.gguf`/HF spec rather than a catalog entry — it is classified non-reasoning, so the kwarg is omitted and the model's chat template can default thinking _on_, emitting unrequested `<think>` blocks to clients that did not ask for them. The catalog path is correct; only the raw escape-hatch path regresses.

**Decision needed:** options are (a) document raw-served hybrids as best-effort and require the catalog for thinking control — no code change, **recommended**; (b) restore an unconditional `enable_thinking:false` for non-reasoning clients, which undoes 4b's deliberate omission and sends the kwarg to genuinely non-reasoning models; (c) add a per-serve reasoning override on the raw path (new surface). Touches the first-class Messages-API drop-in promise (CLAUDE.md rule 3), so it should not silently regress.

## Minor refinements (M2 review)

Low-severity items from the M2 review, none blocking. **Suggested:** opportunistically, when next touching each area.

- **`routeCount` locked 2–3× per dispatched request** (`internal/server/gateway.go`): `ensure`, `Admission.Acquire`'s replica-count callback, and `pick` each take `g.mu` for the same logical decision. Threading the count read once through `dispatchPrep` would drop a couple of RWMutex round-trips from the hot path.
- **OpenAI handler special-cases `ErrNotFound`** (`internal/server/openai.go`): `dispatchPrep` speaks only the Anthropic envelope, so the OpenAI mirror re-maps not-found by hand. Folding the not-found code into `openaiErrType` would let every shed/error type translate uniformly with no per-type branch.
- **MLX served-id re-derived positionally** (`internal/worker/worker.go`): `engineSetup` recovers the served id as `ModelArgs[n-1]`, which `resolve.go` already knows; threading the resolved served name through `worker.Config` (as `ContextWindow`/`Reasoning` already are) removes the positional assumption.
- **`AsyncUsageWriter.Record` block-contract depends on the caller's deadline** (`internal/server/usagewriter.go`): the exported method's "blocks briefly" promise holds only because the sole caller passes a write timeout. The inline-persist fallback was bounded in the M2 review, but a future caller passing `context.Background()` on a full buffer would still block on enqueue; consider an internal enqueue deadline.
- **`Stop` deletes a deployment with waiters>0** (`internal/server/scheduler.go`): an `EnsureModel` waiter can be Stopped out from under. Verified harmless today (each waiter decrements its own captured pointer, so no counter corruption) but untested — a guard or a regression test would lock the behavior in.
