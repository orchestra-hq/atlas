---
title: Observability
description: Prometheus metrics, structured logs, and the atlas status / top CLI.
sidebar:
  order: 3
---

Atlas exposes a Prometheus `/metrics` endpoint and structured logs, plus a terminal inspection tool
for watching a fleet live — the stand-in for the web console (which is its own later milestone).

## Metrics

The gateway serves Prometheus `/metrics`, including a queue-depth / utilization signal usable for
external autoscaling (e.g. an ASG scaling policy). The endpoint requires an admin-scoped key, so
configure your scraper to send it — mint one with `atlas keys create --admin` and pass it as a
`Authorization: Bearer <key>` (or `x-api-key`) header in the Prometheus scrape config. Then build
dashboards as usual.

## CLI inspection

```sh
atlas status   # a snapshot of the fleet
atlas top      # a live view — run over SSH on the gateway box
```

`atlas top` is the "see what the fleet is doing" view from the terminal: workers, deployed models,
in-flight requests, and load.

## Backpressure signals

Under load Atlas sheds with retryable codes rather than timing out — **429 `rate_limit_error`**
(momentarily full) and **529 `overloaded_error`** (no live replica right now), both carrying
`Retry-After`. Watching these alongside queue depth tells you when to add capacity. See
[API compatibility](/atlas/reference/api-compatibility/) and
[Scaling & operations](/atlas/operate/scaling/).
