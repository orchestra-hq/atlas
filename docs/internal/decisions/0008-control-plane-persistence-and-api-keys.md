# ADR-0008: Control-plane persistence and per-client API keys

**Status:** accepted

## Context

Through M0 and M1 phases 1–4 the control plane held no durable state. Client auth was a single shared secret (`atlas server --api-key`, generated if unset); worker join used a single shared `--token`; scheduler desired state was in-memory. Nothing survived a restart, and there was nothing to survive.

M1 phase 5 (API key management → G12) and phase 6 (usage metering → G13) change that. Phase 5 needs a small set of mutable, durable records — API keys with a stored hash, a per-key model allowlist, and an `admin` scope — created and revoked at runtime via `atlas keys`. Phase 6 needs an append-heavy, queryable usage ledger (totals by key, model, worker; survives restart). This is the first state the control plane must persist, so the storage backend is a decision to make once and reuse.

A second, related gap: phase 5's deliverable includes closing the unauthenticated `/admin/*` control surface (worker drain, deploy/scale/stop) that phases 1–4 left open — see the [open question](../open-questions.md#auth-for-the-admin-control-surface-phase-5). The auth model for it is settled here alongside the key system, since both ride the same store.

### Storage backend

Three options were weighed against Atlas's "runs on anyone's infra, just run the binary" promise (ADR-0003 spirit) and its single-binary cross-platform release (GoReleaser, no cgo).

|                           | File store (JSON)      | bbolt (embedded KV) | SQLite via `database/sql`               | Postgres                                 |
| ------------------------- | ---------------------- | ------------------- | --------------------------------------- | ---------------------------------------- |
| Dependency                | none (`encoding/json`) | light, pure-Go      | heavier, pure-Go (`modernc.org/sqlite`) | none in-binary; external server required |
| Single-binary build       | trivial                | trivial             | trivial (pure-Go, no cgo)               | n/a (server is out-of-process)           |
| Operator burden           | none                   | none                | none                                    | high: stand up, secure, back up a DB     |
| Phase-6 usage aggregation | hand-rolled            | hand-rolled         | SQL `GROUP BY`                          | SQL `GROUP BY`                           |
| HA / multi-replica path   | no                     | no                  | swap driver (same `database/sql`)       | native                                   |

**Postgres** is rejected for M1: the control plane is a single `atlas server` process, not a replica set sharing state — scaling in Atlas is horizontal on the _worker_ side, and no control-plane replica reads another's tables. Requiring operators to provision and maintain a separate database server before they can run the control plane directly contradicts the self-hosted promise. Its one real advantage (multi-replica HA) has no caller in M1.

**File / bbolt** are cheaper dependencies but make phase-6 usage aggregation a hand-rolled rollup, and — more importantly — leave no clean migration path if a multi-replica control plane is ever wanted.

## Decision

**Persist control-plane state in SQLite, accessed through `database/sql` with the pure-Go `modernc.org/sqlite` driver,** opened at `<state-dir>/atlas.db`. Persistence lives in a new `internal/db` package (the name `internal/store` is already the content-addressable model-weight cache); the gateway depends only on a small `Authenticator` interface, so the concrete store can be swapped (to Postgres for an eventual HA control plane) without touching the request path. Pure-Go keeps single-binary cross-compilation intact; going through `database/sql` keeps the Postgres door open for free.

**Replace the shared-secret client auth with per-client API keys.** The keys table stores: id, a sha256 hash of the secret, a display prefix, a model allowlist (empty = all models), an `admin` scope flag, and created/revoked timestamps.

API key secrets are hashed with a single **sha256**, not bcrypt. bcrypt exists to slow brute-force of low-entropy human passwords; an Atlas key is a 192-bit machine-generated random token, so a fast hash is not brute-forceable. sha256 also makes the hash a deterministic, **indexed** column — auth is a single index lookup on every `/v1/messages` request, where a per-request bcrypt comparison (no usable index, so an O(n) scan over all keys at a deliberately slow cost factor) would add real latency to the inference hot path. Hashing at all (rather than storing the secret) means a leaked database file does not yield usable keys.

- `atlas keys create [--allow <model>…] [--admin]` mints a key, prints it once (only the hash is stored), and records its allowlist/scope.
- `atlas keys list` shows id, prefix, scope, allowlist, status.
- `atlas keys revoke <id>` invalidates it; revocation takes effect immediately (no cache window).
- The gateway's auth middleware validates `x-api-key` / `Authorization: Bearer` against the store and enforces the per-key model allowlist at resolve time: a key requesting a model outside its allowlist gets a 403, a missing/invalid key a 401, both as Anthropic error envelopes.
- The `--api-key` shared-secret flag is removed. `atlas up` and `atlas server` auto-create a default (full-access, admin) key on first run and print it, so single-node users are not locked out after upgrading.

**Gate `/admin/*` with an admin-scoped key.** The admin control surface (worker list/drain, deploy/scale/stop) requires a key carrying the `admin` scope — option (a) from the open question. This reuses the same store and middleware rather than introducing a second secret or a separate listener. The admin CLI clients (`atlas deploy`/`scale`/`stop`/`workers`) send the key via flag or `ATLAS_API_KEY`.

The worker **join token** is left as-is for now (a single shared `--token`); build-decision 6's hashed `atlas token create/revoke` is deferred to a follow-up that reuses this store (it is a distinct operator secret, not in phase 5's deliverable row).

## Consequences

- The control plane gains a durable, single-file data store with no external dependency and no change to single-binary builds.
- Phase 6's usage ledger reuses the same store and gets SQL aggregation for the G13 totals.
- An eventual HA control plane can move to Postgres by swapping the `store` implementation, not rewriting callers.
- One auth model covers both the client (`/v1/*`) and admin (`/admin/*`) surfaces: scope on the key, not a separate token system.
- Build-decision 4 (API keys replace the shared secret) is realised; build-decision 6 (hashed join tokens) is partially deferred — the join token stays a shared secret until a follow-up folds it into this store.
- Operators upgrading from a shared-secret deployment must switch to the auto-created default key (printed on first start); the old `--api-key` flag is gone. M0 never shipped publicly, so no migration path is provided.
