# ADR-0011: Prefix/session-affinity routing

## Status

accepted

## Context

[ADR-0010](0010-load-balancing-and-backpressure.md) replaced round-robin with least-in-flight replica selection and deferred session/prefix affinity to M3 with intent: least-in-flight optimizes for even load but can move a conversation between replicas turn to turn, forgoing the warm prefix cache a prefix-caching engine maintains. The M2 SGLang adapter (and any engine with RadixAttention-style prefix caching) keeps a per-replica cache of recently-seen prefixes; a conversation that re-lands on the replica that already holds its prefix skips re-prefilling it.

Multi-turn agent loops are exactly the workload that pays for the miss. Claude Code and the Agent SDK re-send the entire growing conversation on every turn, so a conversation routed to a cold replica re-prefills a prompt that grows with the dialogue — the cost compounds precisely as conversations get longer. M3's thesis (point your own agents at your own hardware) makes this the highest-value routing improvement available.

Constraints from prior decisions:

- Routing and admission live in the **gateway** with no new wire protocol ([ADR-0010](0010-load-balancing-and-backpressure.md) §4); affinity must too.
- The Anthropic Messages API is stateless and first-class ([ADR-0002](0002-anthropic-api-first.md)): clients send the full conversation each turn and there is no native session id, so Atlas cannot assume one is supplied.
- Backpressure (429/529) is a contract clients already handle ([ADR-0010](0010-load-balancing-and-backpressure.md) §3); affinity must not undermine it by parking requests on a busy replica.

## Decision

1. **Derive a routing key from the conversation prefix, honoring an explicit session header when present.** The default key is a stable hash of the request's leading prefix (system prompt + the earliest messages, which are what a prefix cache actually shares across turns), so the same conversation produces the same key as it grows. If a client supplies an explicit affinity header (e.g. `x-atlas-session`), that value is the key instead — giving agent frameworks that track a session id a precise hook without requiring one.

2. **Consistent-hash the key to a replica.** The routing key maps to one of the model's replicas by consistent hashing, so adding or removing a replica reshuffles only a fraction of keys rather than remapping every conversation. The implementation uses **rendezvous (highest-random-weight) hashing** keyed on each replica's stable worker id: it delivers the same minimal-reshuffle property without standing up a hash ring or virtual nodes, which suits the small per-model replica sets Atlas selects over, and keying on the stable worker id (not the ephemeral connection id) means a worker cycling its connection does not move keys off it.

3. **Affinity is a hint bounded by load tolerance, never a hard pin.** The affine replica is chosen **only while its in-flight count stays within a configurable tolerance of the least-loaded replica's**. Past the tolerance, selection falls back to plain least-in-flight ([ADR-0010](0010-load-balancing-and-backpressure.md) §1). So affinity wins the warm-cache reuse when capacity is comfortable and yields to load balancing when it is not; it never queues a request behind a busy replica that backpressure would otherwise have spread or shed.

4. **It lives in the ADR-0010 admission/dispatch layer.** Affinity is a selection refinement reading the same per-replica in-flight counters that least-in-flight and the metrics surface already use. No new wire messages, no engine change, no per-conversation server-side state beyond the stateless hash. The queue, slot accounting, and 429/529 shed paths are unchanged.

5. **Configurable, with an off switch.** The load tolerance and the prefix-hash window are configurable; setting the tolerance to disable affinity restores pure least-in-flight (ADR-0010 behavior) for anyone who wants it.

## Consequences

- Multi-turn agent conversations reuse a warm prefix cache on the replica that holds their prefix, cutting re-prefill cost that grows with conversation length — the headline win on a prefix-caching engine like SGLang.
- Because affinity is bounded by load tolerance and rides the existing admission layer, the G16 backpressure semantics are preserved: under load the request still spreads or sheds cleanly, never hangs. Affinity is observable as hit/miss metrics, so the trade-off is measurable rather than assumed.
- The routing key is a stateless hash, so affinity survives a control-plane restart and needs no session store — consistent with the in-memory, single-process control plane (durable/HA routing remains a later milestone).
- Prefix-hash affinity is approximate: two conversations sharing a long system prompt but diverging later hash to the same key and the same replica, which is harmless (they share the cacheable prefix anyway). An explicit session header is exact when a client provides one.
- The benefit is largest on engines with prefix caching and negligible on engines without it; affinity then costs only a hash and a comparison, so it is safe to leave on across the fleet.
