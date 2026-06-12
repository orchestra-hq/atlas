# ADR-0003: Control plane + outbound-dialing workers, with a combined single-node mode

**Status:** accepted

## Context

Atlas must run on a single laptop *and* across a fleet of GPU machines the user controls — including machines on networks the operator can't open inbound ports into (home GPUs, customer VPCs, NAT). The question "is it a job distributor, with a process on each GPU machine and a central process sending jobs?" needed an explicit answer.

## Decision

- **Two roles in one binary:** `atlas server` (control plane: gateway, scheduler, registry, auth, metering, console — no GPU needed) and `atlas worker` (per-machine agent: hardware detection, engine lifecycle, request execution).
- **Workers dial out** to the server with a join token and hold a persistent connection; control messages *and* proxied inference traffic flow over worker-initiated connections. The server never connects in to workers.
- **`atlas up`** runs both roles in one process for single-node use; it is the same code path (in-process worker registration), never a fork of the architecture.
- Scheduling is centralized in the server; workers are deliberately dumb executors.

This matches the converged industry pattern (GPUStack server/worker; CI runner fleets; Anthropic's self-hosted sandbox workers are outbound-only for the same reason).

## Consequences

- Only the server needs a reachable address / TLS cert; workers work from anywhere with outbound internet. This is the foundation of the "your infra, anyone's infra" promise.
- All inference bytes transit the server — acceptable for token streams; a direct-data-plane optimization remains possible later without changing the model.
- The server is a single point of failure in v1; HA control plane is deferred (state is small: registry, keys, assignments — a replicated store can come later).
