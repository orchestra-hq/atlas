---
title: Cloud fleet
description: Run a control plane and dial-out GPU workers across one or many clouds, behind one endpoint.
sidebar:
  order: 4
---

To serve models across several machines behind one endpoint, run `atlas server` on a small always-on
box and `atlas worker --join` on each GPU machine. The same binary and API surface as a single box —
only the topology grows.

## How it fits together

- **Control plane** — `atlas server`, a single Go binary on a small always-on instance (no GPU). Its
  state is one directory (SQLite + config); snapshot it and you've backed up the platform. Put a load
  balancer with a TLS cert in front for a public endpoint (see [TLS](/atlas/operate/tls/)).
- **Workers** — `atlas worker --join` on each GPU machine. Workers **dial out** to the server, so they
  need **no inbound ports** — they sit in private networks with egress only. There's nothing to
  expose and no SSH required to operate.
- **One endpoint** — the server authenticates clients and routes requests across all workers.

## Joining a worker

Joining is non-interactive (flags/env), so it drops straight into instance start-up scripts:

```sh
atlas worker --server https://atlas.yourco.com --join-token "$ATLAS_JOIN_TOKEN"
```

That one line _is_ the integration — install the binary, run it with a join token (kept in your
secrets manager), and the worker appears in the fleet.

## Why this shape is convenient

- **Elastic pools** — because workers self-register and are removed on heartbeat timeout, you can run
  each pool under an autoscaling group: scale out and a new instance joins in minutes; scale in and
  the worker drains in-flight requests, then leaves.
- **Spot-friendly** — an interrupted worker is just a heartbeat timeout; the scheduler re-places its
  models on remaining capacity.
- **Hybrid is free** — since workers dial out, the control plane can be in one place and workers
  anywhere: an on-prem box and a cloud GPU pool joined to the same server, one endpoint. Nothing here
  is cloud-specific.

See [Scaling & operations](/atlas/operate/scaling/) for deploy/scale/stop, drain, and weight caching.

:::note[Concrete recipes are a later milestone]
Turnkey packaging and reference infrastructure-as-code — a Compose file, systemd units, Kubernetes
manifests, and a reference cloud module — are deferred to a demand-driven milestone (M7), so they're
shaped by how operators actually deploy. The dial-out join above works today with the shipped binary
and [installer](/atlas/get-started/installation/); adapt it to your environment's start-up scripts
and autoscaling.
:::
