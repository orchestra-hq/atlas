---
title: Usage metering
description: The durable token ledger, summarized by key, model, and worker.
sidebar:
  order: 2
---

Every completed inference request is recorded in the state-dir database, tagged with the calling key,
the served model, and the worker that ran it. `atlas usage` summarizes the ledger (cumulative since it
was created).

```sh
atlas usage
atlas usage --json   # machine-readable
```

- Totals are **durable across restarts** (they live in the state-dir database).
- A stream interrupted partway (worker drop or client disconnect) still records the output emitted
  before the cut, so the ledger isn't systematically short on interrupted requests.
- **Cloud-fallback** spend, when enabled, is recorded as a distinct usage class — `atlas usage` shows
  it under a `cloud:<provider>` worker, so external spend is visible and separable from local
  capacity. Cloud-fallback is off by default. See
  [API compatibility](/atlas/reference/api-compatibility/).

For live throughput and queue depth rather than cumulative totals, see
[Observability](/atlas/operate/observability/).
