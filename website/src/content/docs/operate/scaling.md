---
title: Scaling & operations
description: Deploy, scale, and stop models; drain workers; spot instances and weights.
sidebar:
  order: 5
---

In a [fleet](/atlas/deploy/cloud-fleet/), models are deployed onto workers and scheduled by the
control plane (VRAM-fit placement, auto-start, idle-stop).

## Deploy, scale, stop

```sh
atlas deploy <model>               # place a model instance on the fleet
atlas scale <model> --replicas N   # scale a deployed model
atlas stop <model>                 # stop a deployment
atlas deployments                  # list deployments
```

See the [CLI reference](/atlas/reference/cli/) for worker management (`atlas workers list`,
`atlas workers remove`).

## Drain, spot, and heartbeats

Workers are stateless executors that self-register and are removed on heartbeat timeout:

- **Graceful drain on SIGTERM** — a worker stops accepting new requests and lets its in-flight
  requests finish before exit, so ASG scale-in and spot interruptions don't drop live requests.
- **Spot instances work naturally** — an interrupted worker is just a heartbeat timeout; the scheduler
  re-places its model instances on remaining capacity.
- **Heartbeat-timeout removal** — vanished workers leave the pool automatically.

## Weights

Each worker caches weights on local NVMe or EBS, so a model is pulled once per worker and subsequent
cold boots are fast. Weights are sourced from Hugging Face or an `https://` GGUF URL (via the model
catalog); pin them to a fast local disk on each worker so you don't re-pull the same 140GB on every
restart.

## Autoscaling

v1 scaling is "you set ASG desired capacity." Atlas exposes a queue-depth / utilization signal on
[`/metrics`](/atlas/operate/observability/) that an external autoscaler (e.g. an ASG policy) can
consume, rather than Atlas calling cloud APIs itself.
