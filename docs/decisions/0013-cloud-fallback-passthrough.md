# ADR-0013: Cloud-fallback passthrough

## Status

accepted

## Context

[ADR-0010](0010-load-balancing-and-backpressure.md) made overload a clean, retryable shed: when a model is past capacity, the gateway returns 429/529 and the client backs off. That is the right default for a self-hosted fleet, but it means a request that the local fleet cannot serve right now is simply not served. Some operators would rather **spill overflow to a real provider** than drop it — keep the agent moving during a capacity spike, or cover a model the fleet has not deployed — and pay the cloud cost for that graceful degradation.

This cuts close to Atlas's identity (point your own agents at your own hardware), so it must be opt-in and honest, never a silent reroute of a user's prompt to a third party. It also touches the drop-in trust promise (CLAUDE.md rule 3): a client must not be misled about where its response came from, and the response must remain byte-for-byte parseable by the SDK.

Because Atlas already canonicalizes every request into the Anthropic and OpenAI shapes, forwarding to an upstream Anthropic or OpenAI endpoint is a near-identity passthrough — the translation work is already done.

## Decision

1. **Cloud-fallback is opt-in and off by default.** It is enabled explicitly per deployment, per model, and/or per key, with operator-supplied upstream credentials. With it disabled — the default — the gateway's behavior is exactly ADR-0010's: overflow sheds with 429/529. No request leaves the operator's infrastructure unless the operator turned this on.

2. **It triggers only where a request would otherwise fail locally.** Two cases: (a) **overflow** — a request that ADR-0010 admission would shed (429/529) is instead forwarded upstream; (b) **unavailable-but-mappable model** — a request for a model the fleet has not deployed but that maps to an upstream model. Normal in-capacity local serving is never diverted.

3. **The upstream call is a passthrough of Atlas's already-canonical request.** The gateway forwards the request to the configured Anthropic or OpenAI endpoint with the operator's key, and streams the response back. A model alias maps to the upstream model name for the spill (e.g. a local `claude-*` alias → a real Anthropic model when configured).

4. **Cloud-served responses are labeled out-of-band, never by editing the body.** The response carries an `x-atlas-served-by: cloud` header (vs. `local`) and its usage is attributed to the cloud ledger class. The response **body is unchanged** — a normal Atlas response the SDK parses identically — so the drop-in promise (CLAUDE.md rule 3) holds. Operators and dashboards can see the spill; the client SDK is unaffected.

5. **Cloud-served tokens are billed as a distinct usage class.** They are real external spend, not local capacity, so the usage ledger records them separately from locally-served tokens, and `atlas usage` distinguishes them. This keeps cost attribution honest and lets an operator cap or alert on cloud spill.

## Consequences

- An operator who opts in gets graceful degradation under a spike (and optional coverage for undeployed models) instead of a hard shed — a real differentiator for agent workloads that would rather slow down than stop.
- The self-hosted promise is preserved by default: nothing spills without an explicit opt-in and explicit upstream credentials, and every cloud-served response is labeled, so a user is never silently routed to a third party.
- The drop-in trust promise is intact because labeling is header/usage-only; SDK parsing of the body is untouched.
- Cloud spend is visible and separable in the ledger, so the feature cannot quietly run up a bill — an operator can monitor and cap it.
- Forwarding leans on Atlas's existing Anthropic/OpenAI canonicalization, so the passthrough is thin; the new surface is credential handling, the enable policy, the label, and the billing class — which is why this lands last in M3, behind the most review.
- Outbound dependency and data-egress implications (prompts leaving the operator's infra to a provider) are the operator's explicit choice, documented at the opt-in; Atlas does not make that choice for them.
- Streaming spill must propagate upstream errors and cancellation back through the same paths local requests use, so a failed upstream call surfaces as a normal error rather than a hang — verified in G22.
