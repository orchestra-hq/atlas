# ADR-0007: WebSocket for the worker channel wire protocol

**Status:** accepted

## Context

M0's worker channel is in-process: `atlas up` registers the worker over a Go channel; the gateway calls `worker.Execute` directly. M1 splits server and worker onto separate machines, so that channel needs a real wire protocol.

The worker channel carries two kinds of traffic: control messages (join, heartbeat, drain) and inference traffic (streamed request + response). Both must flow over worker-initiated connections, because workers must never require inbound connectivity (ADR-0003).

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

| Type            | Direction       | Payload                                                    |
| --------------- | --------------- | ---------------------------------------------------------- |
| `join`          | worker → server | `token`, `hardware` (GPUs, VRAM, RAM, platform), `version` |
| `join_ack`      | server → worker | `accepted`, `worker_id`, `reason`                          |
| `heartbeat`     | worker → server | `worker_id`                                                |
| `heartbeat_ack` | server → worker | —                                                          |
| `execute`       | server → worker | `request_id`, serialised `core.Request`                    |
| `chunk`         | worker → server | `request_id`, serialised `core.StreamEvent`                |
| `done`          | worker → server | `request_id`, `usage`                                      |
| `error`         | worker → server | `request_id`, `code`, `message`                            |
| `drain`         | server → worker | —                                                          |
| `drain_ack`     | worker → server | —                                                          |

`execute`/`chunk`/`done`/`error` carry `internal/core` types — the same representation the gateway uses with the in-process channel — so no new translation layer is needed. Engine adapters stay on the worker side, unchanged.

## Consequences

- Workers connect from behind NAT and corporate proxies without any special network configuration.
- No protobuf schema or code generation in the repo.
- TLS for the `wss://` production endpoint is a separate concern (M1 phase 7: ACME for VPS, self-signed + pinned for private deployments).
- The in-process channel (`atlas up` single-node mode) is kept alongside the WebSocket implementation; both satisfy the same `WorkerChannel` interface. Nothing about single-node mode changes for existing users.
- Scaling limit: all inference bytes transit the server. Acceptable for v1 (token streams are small). A "direct data plane" optimisation (gateway redirects to a worker that chooses to expose a port) can be added later without changing the model — stated in [architecture.md](../architecture.md).
