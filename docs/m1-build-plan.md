# M1 build plan

The ordered path from M0's single-node gateway to a multi-machine fleet. Each phase ends with named conformance groups going green. This refines the M1 milestone in [roadmap.md](roadmap.md); design truth still lives in [architecture.md](architecture.md) and the relevant ADRs.

## Build-time technical decisions

Choices recorded here so they don't get re-litigated mid-build:

1. **WebSocket for the worker channel (ADR-0007).** Workers dial the server's `/workers/connect` WebSocket endpoint and hold the connection open. The in-process channel (`atlas up` single-node mode) is kept alongside the WebSocket implementation behind the same `WorkerChannel` interface.
2. **`execute`/`chunk` messages carry `internal/core` types.** The existing engine adapters stay on the worker side, unchanged. No new translation layer is introduced; the WebSocket protocol transports the same internal representation the in-process channel already uses.
3. **`atlas server` reuses the M0 gateway unchanged.** The M0 gateway (auth, routing, SSE) runs inside `atlas server` without modification. The worker hub is a new component alongside it. `atlas up` keeps the in-process path for single-node users.
4. **API keys replace the shared secret in phase 5.** The `--auth` shared-secret flag is removed; `atlas up` auto-creates a default API key and prints it on first run after upgrade. Running two auth systems in parallel would be confusing and is unnecessary since M0 never shipped publicly.
5. **VRAM estimation uses catalog metadata.** The scheduler estimates required VRAM from the catalog entry: `weight_gb × (1 + kv_overhead_fraction)` where `kv_overhead_fraction ≈ 0.2` for typical context lengths. Conservative: a worker is selected only if its available VRAM meets or exceeds the estimate. Same approach as Ollama.
6. **Join tokens are stored hashed (bcrypt) in SQLite.** `atlas token create` generates a new token; `atlas token revoke <id>` invalidates it. A default token is printed on first `atlas server` start and stored so the operator can retrieve it later.

## Phases

Exit criteria are cumulative: each phase must hold all prior groups green.

| Phase | Deliverable                                                                                                                                                | Exit criterion                                                                                                                                                                                                                                          |
| ----- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1     | `atlas server` standalone control plane; `atlas worker --join <url> --token <token>`; WebSocket join handshake + heartbeat                                 | Workers appear in server; reconnect after drop; `atlas up` unaffected; new `atlas workers list` shows connected workers                                                                                                                                 |
| 2     | Request proxying + SSE streaming over WebSocket channel                                                                                                    | G1–G8+G10 green on genuine two-process deployment (server and worker as separate processes)                                                                                                                                                             |
| 3     | Worker drain on SIGTERM; heartbeat-timeout removal; `atlas workers remove <id>`                                                                            | Drain: in-flight request completes, no new requests accepted; timeout: dead worker removed within the heartbeat timeout (N=3 missed windows, ~30 s), and its in-flight requests return a retryable error rather than hanging until the client times out |
| 4     | Scheduler v1: VRAM-fit placement, connection-identified routes, `atlas deploy/scale/stop`, auto-start + idle-stop                                          | G11 (two workers each running a different model; requests route to the correct worker; reconnect and same-model overlap do not drop a live route)                                                                                                       |
| 5     | API key management: `atlas keys create/list/revoke`, per-key model allowlist; shared secret removed                                                        | G12 (valid key succeeds; missing/invalid key 401; allowlist enforced)                                                                                                                                                                                   |
| 6     | Usage metering: token counts by key/model/worker stored in SQLite; `atlas usage` CLI                                                                       | G13 (usage records exist after N requests; totals correct; queryable by key, model, worker)                                                                                                                                                             |
| 7     | TLS for server endpoint (ACME for public VPS, self-signed + pinned for private); non-interactive join via `ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN` env vars | G14 (fleet ops: `wss://` join works; env-var join works; drain + timeout pass on multi-machine deployment) — **M1 done**                                                                                                                                |

## Phase notes

**Phase 1** introduces the two new commands and the WebSocket hub. `atlas server` starts the control plane only — gateway, scheduler, registry, hub — with no local engine. `atlas worker --join` opens the WebSocket connection, sends a `join` message carrying the join token and the worker's hardware inventory (GPUs, VRAM, RAM, platform), and receives a `join_ack` confirming its `worker_id`. From that point the worker sends `heartbeat` every 10 s; the server replies `heartbeat_ack`. `atlas up` is unchanged — it still starts the in-process channel. The exit criterion does not yet require requests to flow over the channel; it proves the connection, heartbeat, and inventory reporting work.

**Phase 2** is the core M1 engineering challenge: streaming inference bytes through a persistent bidirectional WebSocket connection. When the gateway receives a request for a remote worker, it sends an `execute` message down the channel; the worker calls the local engine and streams `chunk` messages back as the engine produces tokens; the gateway translates the stream back to SSE for the client. One connection multiplexes every in-flight request — a single write pump on each end serialises frames, and a read demux routes responses back to the waiting request by `request_id` — which is the phase's main design work (the realised message set is in ADR-0007). The full G1–G8+G10 suite runs against a two-process deployment on the same machine (server on `:9090`, worker connecting to `ws://localhost:9090/workers/connect`) — same CPU CI runner as M0, no GPU required.

Two interim decisions, both superseded by later phases: until the scheduler lands (phase 4) the worker self-declares the models it serves (`atlas worker --join … --model <m>`, the same engine-loading path as `atlas up`); and until per-key management lands (phase 5) client auth reuses M0's shared secret (`atlas server --api-key`, generated and printed if unset), kept separate from the worker join `--token`.

**Phase 3** closes out the fleet reliability floor. Worker drain: on SIGTERM the worker sends `drain`, the server stops routing new requests to it, in-flight requests complete, then the worker sends `drain_ack` and disconnects. Heartbeat timeout: the server removes a worker that misses N consecutive heartbeat windows (N=3, so ~30 s with 10 s interval). Manual remove: `atlas workers remove <id>` triggers the same drain sequence, and the removed worker's process then **exits** (a clean decommission) rather than reconnecting as a fresh worker — otherwise removal would have no lasting effect while the process is alive. _Future work (revisit post-M1): a "disconnect but keep running" mode, where a removed worker stays up and is suppressed from rejoining, if a use case for it appears._

The heartbeat timeout is also the backstop for a worker that stops answering a request without disconnecting cleanly (crashed but the TCP connection lingers): when the timeout tears the connection down, every in-flight request multiplexed on it must unblock with `ErrEngineUnavailable` rather than waiting on the client's request deadline. This is a review finding (the non-streaming demux loops in `remote.go` have no per-request deadline of their own); the timeout teardown is the right layer to bound it, so Phase 3's exit criterion asserts it directly — `kill -9` the worker and confirm an in-flight `/v1/messages` returns a retryable error within the heartbeat timeout (N=3 missed windows, ~30 s). The connection's existing read deadline (`heartbeatTimeout` in `internal/server/hub.go`) already is this backstop: when it fires the hub read loop returns and `remoteWorker.close` unblocks every waiting request — so the fix was to assert the behavior rather than add a new mechanism.

**Phase 4** makes the scheduler a real placement engine. In M0 the scheduler was trivial — one worker, one model. In M1 it tracks worker hardware inventories (from `join` messages), estimates VRAM requirements from the catalog, and picks the best-fit worker when placing a model instance. `atlas deploy <model> [--worker <id>]` places a model manually or lets the scheduler choose; `atlas scale <model> --replicas N` adjusts; `atlas stop <model>` shuts it down. Auto-start on first request and idle-stop after N idle minutes mirror the M0 single-node behavior, generalized to the fleet. The G11 exit criterion runs the full G1–G8+G10 suite twice — once per worker — proving that the gateway routes each model to the correct machine.

Phase 4 lands in two PRs, because the routing rewrite that G11 gates on is independent of the much larger remote-model-loading machinery:

- **Phase 4a (this tranche) — connection-identified routing → G11.** Replaces the gateway's name→single-`Exec` table with an instance model: a model name resolves to a set of live `(worker-connection, executor)` routes, requests round-robin across a model's replicas, and a worker's teardown removes only the instances it owns. Workers keep self-declaring their models (`--model`, as in phase 2); this tranche adds no new wire protocol. It satisfies G11 in full.
- **Phase 4b — placement + lifecycle.** Itself split in two: **4b-1** adds remote model loading (the `load`/`unload`/`model_ready`/`model_unloaded`/`load_failed` wire messages + worker-side dynamic engine launch), the VRAM-fit scheduler, and `atlas deploy`/`scale`/`stop`/`deployments`; **4b-2** adds auto-start on first request and idle-stop on top of it. VRAM-fit _placement_ has no caller until 4b-1 — there is nothing to place while workers self-declare. Replica selection in 4a is round-robin; load-aware selection is a later refinement.

  Phase 4b-1 detail: a worker now provisions its engine at startup and accepts `load` commands from the scheduler, launching a model through the same path `atlas worker --model` uses and reporting `model_ready` (the gateway then registers the route exactly as a join-time model). The scheduler holds desired state (model → replicas, in-memory) and an observed view of each worker's loaded/pending models, and reconciles them: it places a replica on the matching-engine worker with the most free capacity that fits the model's estimated need (`weight_size × 1.2` from the catalog; GPU VRAM for accelerated workers, RAM otherwise), re-places replicas when a worker leaves, and retries elsewhere on a failed load. Models a worker loads on demand are torn down when its connection ends, so a reconnecting worker re-loads under reconcile. `atlas deploy <model> [--replicas N] [--worker <id>]`, `atlas scale <model> --replicas N`, `atlas stop <model>`, and `atlas deployments` drive it over `/admin/deployments`. Pre-declared `--model` instances still work and count toward replicas but are never unloaded by the scheduler.

  Phase 4b-2 detail: the gateway no longer 404s a request for a catalog model that has no live route. Instead it calls the scheduler's auto-start hook, which deploys one replica (reusing 4b-1's placement and `load` path) and blocks the request until an instance reports ready — or gives up fast if the model is unknown, nowhere in the fleet can host it, or the wait exceeds `--autostart-timeout` (default 5m, `0` disables). Auto-started deployments are marked distinct from operator `atlas deploy`s: a background reaper unloads any auto-started deployment that goes untouched for `--idle-timeout` (default 15m, `0` disables), while operator deployments stay until `atlas stop`. The gateway records activity on every routed request, so a steadily used auto-started model is never reaped; an explicit `atlas deploy` of an auto-started model takes ownership of it (no longer idle-reapable). Auto-start fires only on the inference surfaces (`/v1/messages`, `/v1/chat/completions`), not on metadata endpoints (`count_tokens`, model listing). No new wire messages — it is built entirely on 4b-1's `load`/`unload` protocol.

Phase 4a also fixes a review finding in the registry: M0/phase-2 routes are keyed by model name alone, with no connection identity, so a worker that blips and reconnects (or a second worker advertising the same model) lets the first connection's deferred unregister delete the route the live connection installed — the model goes 404 with a healthy worker still connected. Connection-identified routes are exactly the fix: a worker's teardown removes only the entries it owns, and a name resolves as long as any live instance serves it. G11 gains two cases that exercise this — a worker reconnecting mid-flight, and two workers serving the same model name — neither of which should drop a live route.

**Phase 5** replaces the single shared secret introduced in M0 with proper per-client API keys. The gateway's auth middleware is updated to validate against the SQLite keys table; the `--auth` flag is removed. `atlas up` auto-creates a default key on first run and prints it so single-node users are not locked out after upgrading. The G12 group verifies the full auth contract: valid key, invalid key, missing key, and per-key model allowlist enforcement.

**Phase 6** adds durable usage records. Every completed request writes a row to SQLite: timestamp, key ID, model, worker ID, input tokens, output tokens. `atlas usage` renders a summary table. G13 verifies that records accumulate correctly and that totals match the `usage` fields on individual responses (consistent with the G1 assertion that `usage` is populated).

A review finding to settle while this code is being written: today the gateway records usage only after a stream finishes cleanly — a worker drop or client-write failure mid-stream takes the error path and skips both `recordUsage` and the closing `message_delta` usage emission, so every interrupted stream silently logs zero output tokens despite having produced some. As Phase 6 moves usage onto a durable path, record the tokens accumulated so far on the error/cancel path too (the streaming sink already holds a running count), so the ledger is not systematically short on interrupted requests. G13 gains an interrupted-stream case: kill the stream partway and assert the recorded usage reflects the tokens actually emitted.

**Phase 7** closes M1. TLS is split into two paths: ACME (Let's Encrypt) for servers with a public DNS name (the common VPS case), and self-signed cert with optional pinning on the worker side for private deployments. Non-interactive join allows `atlas worker` to read its server URL and token from environment variables (`ATLAS_SERVER_URL`, `ATLAS_JOIN_TOKEN`), making container and systemd deployments scriptable without flag plumbing. The G14 group exercises these on a genuine two-machine deployment — a server with a TLS cert, a worker connecting from a separate host — and replays the drain + timeout scenarios from G-fleet-ops.

## Conformance groups added in M1

These extend the suite defined in [conformance-suite.md](conformance-suite.md).

### G11 — Multi-worker routing

Two workers registered, each running a different model. Requests for model A route to worker A; requests for model B route to worker B. G1–G8+G10 pass against both workers independently. A request for an undeployed model returns a well-formed 503.

Route-identity cases (review finding): (1) a worker holding model A reconnects (drop + rejoin) while the old connection's teardown is still in flight — model A stays routable throughout, never transiently 404s. (2) Two workers both advertise model A; when one disconnects, requests for A continue to be served by the other. Both fail on a name-keyed registry and pass only once routes carry connection/worker identity.

### G12 — Auth

Missing `x-api-key` → 401 Anthropic error envelope; invalid key → 401; valid key → request succeeds. Key with model allowlist: allowed model succeeds, disallowed model → 403. Key revocation takes effect immediately (no cache window).

### G13 — Usage metering

After N requests across two keys and two models: `atlas usage` returns correct per-key and per-model totals; input + output token sums match the `usage` fields returned on individual responses; records survive process restart (durable in SQLite). Interrupted-stream case (review finding): a stream cut off partway (worker drop or client disconnect) still records the output tokens emitted up to the cut, rather than zero.

### G14 — Fleet ops

`wss://` connection works end-to-end (server with TLS cert, worker connecting over `wss://`). Env-var join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`) works without CLI flags. Worker drain: SIGTERM triggers drain, in-flight request completes, no new requests routed to draining worker. Heartbeat timeout: kill -9 on worker process; server marks it gone within 2× heartbeat interval, and a request that was in flight to it returns a retryable error within the same window rather than hanging until the client's own timeout.

## Testing tiers

M1 reuses M0's tier structure with one addition:

| Tier        | What                                                             | When              |
| ----------- | ---------------------------------------------------------------- | ----------------- |
| Unit        | WebSocket message framing, scheduler VRAM-fit logic, key hashing | Every PR          |
| Integration | Gateway + worker hub against a fake WebSocket worker stub        | Every PR          |
| Conformance | Real llama.cpp, two-process local deployment (phases 2+)         | Every PR          |
| Full matrix | Both engines, GPU, multi-machine deployment                      | Nightly + release |

The conformance tier runs server and worker as separate processes on the same CI runner — no second machine required. The full matrix adds a second real machine for G14.

## Review findings folded into M1

A code review after phase 2 surfaced a set of correctness and hardening issues. Those that sit on a path a later phase already rebuilds are folded into that phase's work and acceptance, rather than patched in isolation:

- **Name-keyed routes lose a live route on reconnect / same-model overlap** → _resolved in Phase 4a_ (connection-identified routes): the gateway keys routes by worker connection, so a teardown removes only that connection's instances. Asserted by `TestReconnectOverlapKeepsLiveRoute`, `TestSameModelTwoWorkersKeepsRoute`, `TestHub_sameModelTwoConnectionsKeepsRoute`, and the two-worker G11 CI scenario.
- **Interrupted streams record zero usage** → Phase 6 (record partial usage on the error/cancel path) + the new G13 case.
- **A crashed-but-connected worker hangs in-flight requests until the client times out** → _resolved in Phase 3_: the heartbeat-timeout teardown unblocks them, asserted directly by `TestHub_timeoutUnblocksInflight` and the fleet-ops CI scenario.

Findings with no natural home in phases 3–7 (the multiplexed-connection backpressure, frame-size and engine-response memory bounds, the worker-side cancellation path, the count_tokens cancellation gap, the cached-blob integrity check, and the buffered stop-sequence block-loss bug) were fixed directly as a standalone connection-hardening change rather than deferred.
