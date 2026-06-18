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

| Phase | Deliverable                                                                                                                                                | Exit criterion                                                                                                           |
| ----- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| 1     | `atlas server` standalone control plane; `atlas worker --join <url> --token <token>`; WebSocket join handshake + heartbeat                                 | Workers appear in server; reconnect after drop; `atlas up` unaffected; new `atlas workers list` shows connected workers  |
| 2     | Request proxying + SSE streaming over WebSocket channel                                                                                                    | G1–G8+G10 green on genuine two-process deployment (server and worker as separate processes)                              |
| 3     | Worker drain on SIGTERM; heartbeat-timeout removal; `atlas workers remove <id>`                                                                            | Drain: in-flight request completes, no new requests accepted; timeout: dead worker removed within 2× heartbeat interval  |
| 4     | Scheduler v1: VRAM-fit placement, `atlas deploy/scale/stop`, auto-start + idle-stop                                                                        | G11 (two workers each running a different model; requests route to the correct worker)                                   |
| 5     | API key management: `atlas keys create/list/revoke`, per-key model allowlist; shared secret removed                                                        | G12 (valid key succeeds; missing/invalid key 401; allowlist enforced)                                                    |
| 6     | Usage metering: token counts by key/model/worker stored in SQLite; `atlas usage` CLI                                                                       | G13 (usage records exist after N requests; totals correct; queryable by key, model, worker)                              |
| 7     | TLS for server endpoint (ACME for public VPS, self-signed + pinned for private); non-interactive join via `ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN` env vars | G14 (fleet ops: `wss://` join works; env-var join works; drain + timeout pass on multi-machine deployment) — **M1 done** |

## Phase notes

**Phase 1** introduces the two new commands and the WebSocket hub. `atlas server` starts the control plane only — gateway, scheduler, registry, hub — with no local engine. `atlas worker --join` opens the WebSocket connection, sends a `join` message carrying the join token and the worker's hardware inventory (GPUs, VRAM, RAM, platform), and receives a `join_ack` confirming its `worker_id`. From that point the worker sends `heartbeat` every 10 s; the server replies `heartbeat_ack`. `atlas up` is unchanged — it still starts the in-process channel. The exit criterion does not yet require requests to flow over the channel; it proves the connection, heartbeat, and inventory reporting work.

**Phase 2** is the core M1 engineering challenge: streaming inference bytes through a persistent bidirectional WebSocket connection. When the gateway receives a request for a remote worker, it sends an `execute` message down the channel; the worker translates it, calls the local engine, and streams `chunk` messages back as the engine produces tokens; the gateway translates the stream back to SSE for the client. The full G1–G8+G10 suite runs against a two-process deployment on the same machine (server on `:9090`, worker connecting to `ws://localhost:9090/workers/connect`) — same CPU CI runner as M0, no GPU required.

**Phase 3** closes out the fleet reliability floor. Worker drain: on SIGTERM the worker sends `drain`, the server stops routing new requests to it, in-flight requests complete, then the worker sends `drain_ack` and disconnects. Heartbeat timeout: the server removes a worker that misses N consecutive heartbeat windows (N=3, so ~30 s with 10 s interval). Manual remove: `atlas workers remove <id>` triggers the same drain sequence.

**Phase 4** makes the scheduler a real placement engine. In M0 the scheduler was trivial — one worker, one model. In M1 it tracks worker hardware inventories (from `join` messages), estimates VRAM requirements from the catalog, and picks the best-fit worker when placing a model instance. `atlas deploy <model> [--worker <id>]` places a model manually or lets the scheduler choose; `atlas scale <model> --replicas N` adjusts; `atlas stop <model>` shuts it down. Auto-start on first request and idle-stop after N idle minutes mirror the M0 single-node behavior, generalized to the fleet. The G11 exit criterion runs the full G1–G8+G10 suite twice — once per worker — proving that the gateway routes each model to the correct machine.

**Phase 5** replaces the single shared secret introduced in M0 with proper per-client API keys. The gateway's auth middleware is updated to validate against the SQLite keys table; the `--auth` flag is removed. `atlas up` auto-creates a default key on first run and prints it so single-node users are not locked out after upgrading. The G12 group verifies the full auth contract: valid key, invalid key, missing key, and per-key model allowlist enforcement.

**Phase 6** adds durable usage records. Every completed request writes a row to SQLite: timestamp, key ID, model, worker ID, input tokens, output tokens. `atlas usage` renders a summary table. G13 verifies that records accumulate correctly and that totals match the `usage` fields on individual responses (consistent with the G1 assertion that `usage` is populated).

**Phase 7** closes M1. TLS is split into two paths: ACME (Let's Encrypt) for servers with a public DNS name (the common VPS case), and self-signed cert with optional pinning on the worker side for private deployments. Non-interactive join allows `atlas worker` to read its server URL and token from environment variables (`ATLAS_SERVER_URL`, `ATLAS_JOIN_TOKEN`), making container and systemd deployments scriptable without flag plumbing. The G14 group exercises these on a genuine two-machine deployment — a server with a TLS cert, a worker connecting from a separate host — and replays the drain + timeout scenarios from G-fleet-ops.

## Conformance groups added in M1

These extend the suite defined in [conformance-suite.md](conformance-suite.md).

### G11 — Multi-worker routing

Two workers registered, each running a different model. Requests for model A route to worker A; requests for model B route to worker B. G1–G8+G10 pass against both workers independently. A request for an undeployed model returns a well-formed 503.

### G12 — Auth

Missing `x-api-key` → 401 Anthropic error envelope; invalid key → 401; valid key → request succeeds. Key with model allowlist: allowed model succeeds, disallowed model → 403. Key revocation takes effect immediately (no cache window).

### G13 — Usage metering

After N requests across two keys and two models: `atlas usage` returns correct per-key and per-model totals; input + output token sums match the `usage` fields returned on individual responses; records survive process restart (durable in SQLite).

### G14 — Fleet ops

`wss://` connection works end-to-end (server with TLS cert, worker connecting over `wss://`). Env-var join (`ATLAS_SERVER_URL` + `ATLAS_JOIN_TOKEN`) works without CLI flags. Worker drain: SIGTERM triggers drain, in-flight request completes, no new requests routed to draining worker. Heartbeat timeout: kill -9 on worker process; server marks it gone within 2× heartbeat interval.

## Testing tiers

M1 reuses M0's tier structure with one addition:

| Tier        | What                                                             | When              |
| ----------- | ---------------------------------------------------------------- | ----------------- |
| Unit        | WebSocket message framing, scheduler VRAM-fit logic, key hashing | Every PR          |
| Integration | Gateway + worker hub against a fake WebSocket worker stub        | Every PR          |
| Conformance | Real llama.cpp, two-process local deployment (phases 2+)         | Every PR          |
| Full matrix | Both engines, GPU, multi-machine deployment                      | Nightly + release |

The conformance tier runs server and worker as separate processes on the same CI runner — no second machine required. The full matrix adds a second real machine for G14.
