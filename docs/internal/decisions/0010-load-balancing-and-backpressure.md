# ADR-0010: Load balancing and backpressure

## Status

accepted

## Context

M1 routes requests across a model's replicas by round-robin (`resolveLocked` in `internal/server/gateway.go`). Two gaps surface under real load, and M2 ("operate a real fleet") has to close both:

1. **Round-robin ignores how busy each replica is.** A long or slow request occupying one replica does not stop the rotation from handing it the next request, which inflates tail latency when replicas drift out of balance (uneven request cost, a cold replica, a wedged engine).
2. **There is no capacity ceiling.** When every replica is saturated the gateway keeps forwarding, so excess load turns into engine-side queue blowup, client timeouts, and unshaped 5xx. There is no graceful "we are full, retry shortly" signal.

Constraints from prior decisions:

- The Anthropic Messages API is first-class ([ADR-0002](0002-anthropic-api-first.md)), so an overload response must use the Anthropic error envelope and the status codes the SDK already retries; the OpenAI surface must mirror it.
- Workers dial out and one connection multiplexes every in-flight request ([ADR-0003](0003-control-plane-worker-split.md), [ADR-0007](0007-websocket-worker-channel.md)), so the **gateway** is the natural place to count in-flight work per replica and to admit or shed.

## Decision

1. **Load-aware replica selection: least-in-flight.** Replace round-robin with picking the replica that currently has the fewest in-flight requests (ties broken randomly). Each route gains an in-flight counter, incremented at dispatch and decremented at completion — including the error and cancel paths. Least-in-flight is chosen over latency-EWMA schemes because it needs no latency bookkeeping in the hot path, naturally accounts for heterogeneous request cost, and is the standard cheap choice. Session- or prefix-affinity routing (sticking a conversation to a warm replica) is explicitly **out of scope** here — it is a roadmap M3 concern (SGLang prefix-cache synergy).

2. **Bounded admission queue per model, then shed.** Each model has a maximum concurrency equal to the sum of its replicas' slot counts (a per-replica concurrency limit, defaulted from flags/catalog until engines report a real limit). When all slots are busy, an incoming request waits in a bounded FIFO queue with a maximum length and a maximum wait. A freed slot dequeues the oldest waiter; a request that cannot be admitted (queue full, or max wait exceeded) is shed rather than forwarded.

3. **Overload responses — 429 vs 529, both retryable.**
   - **429 `rate_limit_error`** — the model has live capacity but is momentarily full (queue full or max wait exceeded). The client should retry with backoff; the response carries `Retry-After`.
   - **529 `overloaded_error`** — the system cannot serve at all right now (no live replica can take the model and auto-start cannot place one, or the fleet is hard-saturated past policy).
     Both are emitted in the Anthropic error envelope, mirrored on the OpenAI surface. This is distinct from **404** (unknown/undeployed model — unchanged). The Claude Agent SDK and OpenAI SDK already back off on 429/529, so drop-in clients handle overload for free.

4. **It lives in the gateway, with no new wire protocol.** An admission/dispatch layer sits between resolution and execution; it owns per-model slot accounting, the queue, and the in-flight counters that feed both selection and the metrics surface. Workers and engines are unchanged. The queue is in-memory — the control plane is single-process in M2 (HA is M3).

5. **Configurable, with conservative defaults and an off switch.** Per-replica concurrency, queue length, and max queue wait are configurable (flags, catalog) with safe defaults; setting them to `0`/disabled restores M1's forward-everything behavior for anyone who wants it.

## Consequences

- Overload becomes a clean, retryable 429/529 with `Retry-After` instead of hangs and 5xx — and because the SDKs already retry these, no client change is needed.
- Least-in-flight needs only an atomic counter per route; no latency histograms on the hot path.
- The in-flight counters, queue depth, and shed counts are the **same series the observability phase exposes** via `/metrics` and `atlas top`, so the backpressure conformance group (G16) is verified through the observability surface built in the prior phase — the two phases reinforce each other.
- An in-memory queue means a control-plane restart drops not-yet-dispatched queued requests; this is acceptable (they are un-acked, clients retry). Durable/HA admission is M3.
- Session/prefix affinity is deliberately absent: least-in-flight can move a conversation between replicas, forgoing warm-cache reuse. Revisit in M3.
- Per-model max concurrency rests on a per-replica slot count that is a configured default until engines expose a real concurrency limit; revisit when they do.
