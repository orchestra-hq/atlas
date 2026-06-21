# Follow-ups

Deferred, non-blocking work surfaced by code reviews — items intentionally **not** done in the milestone that surfaced them, parked here so signing a milestone "code-complete" never buries a known refinement inside that milestone's build plan.

Scope rules:

- Correctness and security findings are fixed in-milestone, not deferred here. This file holds only **non-blocking refinements** (efficiency, altitude, operability, UX) and the design decisions a few of them need before they can be done.
- Each entry states what, why it was deferred, where in the code it lives, a suggested milestone, and the review that surfaced it. Items needing an owner decision before work can start are marked **Decision needed** (those are the ones that would otherwise go in [open-questions.md](open-questions.md); they live here so all review fallout sits in one place).
- Delete an entry when it ships — git history keeps the trail.

## Usage metering

### Usage `worker_id` is the ephemeral connection id, not the stable worker name

**Suggested:** M2 (usage in the web console, where per-worker accounting becomes user-facing). **Surfaced:** post-phases-6–7 review.

The ledger's `WorkerID` is the per-connection hub id (`newHubWorkerID`, `w_<hex>` in `internal/server/hub.go`), regenerated on every join. So `atlas usage --by-worker` fragments one physical machine's totals across many ids over its reconnects, and none matches the `Name` shown by `atlas workers list`. Per-worker accounting is unusable for any worker that ever reconnects.

The plumbing fix: thread the worker's stable `--name` (`hubWorker.Name`) through `InstanceRegistry.RegisterInstance` into the `route`, and have `resolve` return it for usage attribution (the connection id stays the routing/teardown key and the `atlas workers` / `deploy --worker` handle).

**Decision needed:** what is the ledger's worker identity? (a) the operator-supplied `--name`, accepting that two workers sharing a name merge in `--by-worker` totals (a name _is_ the operator's stable identity for a box) — recommended; (b) require names unique per server and reject a colliding join; or (c) a server-assigned stable id persisted in the store and reused across reconnects (most robust, most machinery). The choice determines whether `--name` must be validated for uniqueness at join time.

### Per-request usage write is a synchronous SQLite `INSERT` on the hot path

**Suggested:** M2 (alongside "basic queueing/backpressure" + observability). **Surfaced:** post-phases-6–7 review.

`withRequestLog` (`internal/server/ops.go`) writes one ledger row per billable request inline in the request goroutine, serialized through SQLite's single WAL writer. Under fleet-scale concurrency this becomes a writer bottleneck (goroutines blocking on the 5 s busy timeout). Acceptable at M1's scale; the scale fix is a buffered async writer — push `UsageRecord`s onto a channel and have one background goroutine batch multi-row inserts per flush, decoupling the hot path from disk.

## TLS / transport security

### Admin CLI cannot pin a self-signed / private cert

**Suggested:** M2 (or any focused TLS-completeness change). **Surfaced:** post-phases-6–7 review.

`atlas worker` has full `--tls-pin` plumbing, but the admin clients (`atlas deploy`/`scale`/`stop`/`workers`, all using `http.DefaultClient` in `internal/cli/deploy.go` and `internal/cli/workers.go`) validate against the system trust store with no pin or insecure option. So the `https://` deploy command the `--tls-self-signed` banner prints fails with `x509: certificate signed by unknown authority` unless the operator installs the cert into the OS trust store. The server banner currently states this caveat explicitly. The real fix: a shared `--tls-pin` (+ `ATLAS_TLS_PIN`) flag and a pinned `*http.Client` (reusing `tlsx.PinnedVerifier`) across the ~6 admin call sites.

### Self-signed cert cache ignores changed `--tls` hosts

**Suggested:** M2. **Surfaced:** post-phases-6–7 review.

`loadOrCreateSelfSigned` (`internal/cli/tls.go`) returns the cached `cert.pem`/`key.pem` whenever both exist, without checking the requested SAN hosts still match. A host/hostname change keeps serving a cert whose SANs omit the new host, so a non-pinning client fails hostname validation until the files are deleted by hand. Low impact while pinned workers (which skip the name check) are the primary client.

**Decision needed:** the obvious fix — regenerate on SAN mismatch — changes the pin and breaks already-distributed worker pins. Pick: warn-and-keep, error-and-instruct (tell the operator to delete the cached files), or regenerate-and-reprint the new pin.

### ACME mode does not reconcile `--tls-acme-domain` with `--addr`

**Suggested:** M2 (when ACME is exercised on a real public deployment). **Surfaced:** post-phases-6–7 review.

TLS-ALPN-01 validation requires the server reachable on `:443`, but nothing checks the listen port, so `atlas server --tls-acme-domain …` left on the default `:9090` silently never obtains a cert (every handshake fails fetching one). Only a banner note states the `:443` requirement today (`internal/cli/tls.go`, `acmeTLS`). A hard check is wrong — operators legitimately sit behind an LB/port-forward doing `:443 → :9090` — so the right move is a startup _warning_ when the listen port is not 443 and no proxy is declared.

## Scheduler / lifecycle

### Auto-start readiness signalling (busy-poll → event)

**Suggested:** M2. **Surfaced:** post-phase-4 review.

`EnsureModel` busy-polls `placementState` every 50 ms under the scheduler lock while a model loads. Replace the poll with a per-model readiness signal (a `chan`/`sync.Cond` fired from `ModelReady`/`LoadFailed`) so a waiter blocks on an event instead of re-deriving the fit picture 20×/s and contending the lock the loading callbacks need.

### Single resolve-with-intent entry point

**Suggested:** M2. **Surfaced:** post-phase-4 review.

"Auto-start fires on inference surfaces only" is currently enforced by which handler calls `resolveOrStart` vs `resolve` — a copy-paste hazard. Fold the policy into one resolve entry point that takes an explicit intent, so a future handler cannot silently get the wrong behavior.
