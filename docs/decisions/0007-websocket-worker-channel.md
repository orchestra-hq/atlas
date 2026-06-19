# ADR-0007: WebSocket for the worker channel wire protocol

**Status:** accepted

## Context

M0's worker channel is in-process: `atlas up` registers the worker over a Go channel; the gateway calls `worker.Execute` directly. M1 splits server and worker onto separate machines, so that channel needs a real wire protocol.

The worker channel carries two kinds of traffic: control messages (join, heartbeat, drain, load/unload placement) and inference traffic (streamed request + response). Both must flow over worker-initiated connections, because workers must never require inbound connectivity (ADR-0003).

Two protocols were evaluated:

|                                  | gRPC                                                                                                                    | WebSocket                                                          |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| Transport                        | HTTP/2                                                                                                                  | HTTP/1.1 upgrade                                                   |
| Bidirectional streaming          | Native                                                                                                                  | Native (after upgrade)                                             |
| Schema                           | Protobuf (codegen required)                                                                                             | None (JSON text frames)                                            |
| Proxy / corporate network compat | Poor: h2c (cleartext HTTP/2) unsupported by most proxies and AWS ALB; gRPC effectively requires TLS + HTTP/2 end-to-end | Good: HTTP/1.1 CONNECT tunneling passes through almost every proxy |
| Go stdlib support                | External dependency (`google.golang.org/grpc`)                                                                          | `net/http` upgrader + `gorilla/websocket` or stdlib                |
| Reconnect                        | Manual exponential-backoff on top of gRPC                                                                               | Standard HTTP retry                                                |

The central constraint is the "join from anywhere" promise: workers behind NAT, home routers, and corporate proxies must connect without special port configuration or TLS infrastructure on the worker side. gRPC's HTTP/2 requirement is incompatible with this — most corporate middleboxes and load balancers do not pass h2c, so gRPC effectively forces TLS end-to-end, which in turn requires the server to have a valid cert before any worker can join. WebSocket's HTTP/1.1 upgrade works through all of these transparently.

## Decision

Use WebSocket (`ws://` for dev/internal, `wss://` for production). The server exposes a `/workers/connect` WebSocket endpoint; workers dial it on start and hold the connection open.

**Message protocol** — JSON text frames, envelope `{"type":"…","id":"…","payload":{…}}`:

| Type             | Direction       | Payload                                                              |
| ---------------- | --------------- | -------------------------------------------------------------------- |
| `join`           | worker → server | `token`, `hardware` (GPUs, VRAM, RAM, platform), `version`, `models` |
| `join_ack`       | server → worker | `accepted`, `worker_id`, `reason`                                    |
| `heartbeat`      | worker → server | `worker_id`                                                          |
| `heartbeat_ack`  | server → worker | —                                                                    |
| `execute`        | server → worker | `request_id`, `stream`, serialised `core.Request`                    |
| `count_tokens`   | server → worker | `request_id`, serialised `core.Request`                              |
| `cancel`         | server → worker | `request_id`                                                         |
| `response`       | worker → server | `request_id`, serialised `core.Response` (non-streaming reply)       |
| `chunk`          | worker → server | `request_id`, serialised `core.StreamEvent`                          |
| `done`           | worker → server | `request_id`, `stop_reason`, `usage`                                 |
| `token_count`    | worker → server | `request_id`, `count`                                                |
| `error`          | worker → server | `request_id`, `code`, `message`                                      |
| `drain`          | worker ↔ server | —                                                                    |
| `drain_ack`      | worker → server | —                                                                    |
| `load`           | server → worker | `model`, `engine`                                                    |
| `unload`         | server → worker | `model`                                                              |
| `model_ready`    | worker → server | `model`, `context_window`                                            |
| `model_unloaded` | worker → server | `model`                                                              |
| `load_failed`    | worker → server | `model`, `reason`                                                    |

The inference messages carry `internal/core` types — the same representation the gateway uses with the in-process channel — so no new translation layer is needed. Engine adapters stay on the worker side, unchanged.

The gateway's worker view is three methods (`Execute`, `ExecuteStream`, `CountTokens`), so the protocol mirrors them rather than collapsing to a single streamed path: `execute` with `stream:false` is answered by `response`, `execute` with `stream:true` by a `chunk*` then `done` sequence, and `count_tokens` by `token_count`. This preserves in-process fidelity — a remote worker drives the same adapter method the in-process worker would. A worker advertises the models it serves in `join.models` (name + context window); the gateway registers a route per model on join and removes it on disconnect. `cancel` stops an in-flight request when the client disconnects or a stop sequence matches mid-stream, so the worker does not keep generating into a stream no one is reading. `done`/`response`/`token_count` were elaborated from the original `execute`/`chunk`/`done`/`error` sketch when M1 phase 2 was built (count_tokens and the buffered reply had no message in the first cut).

`drain` is bidirectional (M1 phase 3): a worker sends it on `SIGTERM` to announce it is leaving, and the server sends it to evict a worker (`atlas workers remove`). Either way the server stops routing new requests to that worker while its in-flight requests finish; the worker then sends `drain_ack` (always worker → server, terminal) and disconnects. A worker that crashes without draining is caught by the heartbeat timeout instead: when the server tears that connection down it unblocks every request still multiplexed on it with a retryable error, rather than leaving them to hang until the client's own deadline.

`load`/`unload` carry scheduler-driven placement (M1 phase 4b): the server tells a worker to launch or stop a model on demand, and the worker answers `model_ready` (with the engine's real context window, so the gateway registers the route exactly as a join-time served model), `model_unloaded`, or `load_failed`. The worker resolves the model spec, fetches weights, and boots the engine through the same path `atlas worker --model` uses; models a connection loads this way are torn down when it ends, so the scheduler re-places them on reconnect. The connection identifies the worker on both ends, so these frames carry only the model name (and engine, which the scheduler set when it chose a matching-engine worker).

## Consequences

- Workers connect from behind NAT and corporate proxies without any special network configuration.
- No protobuf schema or code generation in the repo.
- TLS for the `wss://` production endpoint is a separate concern (M1 phase 7: ACME for VPS, self-signed + pinned for private deployments).
- The in-process channel (`atlas up` single-node mode) is kept alongside the WebSocket implementation; both satisfy the same `WorkerChannel` interface. Nothing about single-node mode changes for existing users.
- Scaling limit: all inference bytes transit the server. Acceptable for v1 (token streams are small). A "direct data plane" optimisation (gateway redirects to a worker that chooses to expose a port) can be added later without changing the model — stated in [architecture.md](../architecture.md).
