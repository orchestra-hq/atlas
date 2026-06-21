# Follow-ups

Deferred, non-blocking work surfaced by code reviews — items intentionally **not** done in the milestone that surfaced them, parked here so signing a milestone "code-complete" never buries a known refinement inside that milestone's build plan.

Scope rules:

- Correctness and security findings are fixed in-milestone, not deferred here. This file holds only **non-blocking refinements** (efficiency, altitude, operability, UX) and the design decisions a few of them need before they can be done.
- Each entry states what, why it was deferred, where in the code it lives, a suggested milestone, and the review that surfaced it. Items needing an owner decision before work can start are marked **Decision needed** (those are the ones that would otherwise go in [open-questions.md](open-questions.md); they live here so all review fallout sits in one place).
- Delete an entry when it ships — git history keeps the trail.

## TLS / transport security

### Self-signed cert cache ignores changed `--tls` hosts

**Suggested:** M5 (packaging & deployment — bites on real public deploys). **Surfaced:** post-phases-6–7 review.

`loadOrCreateSelfSigned` (`internal/cli/tls.go`) returns the cached `cert.pem`/`key.pem` whenever both exist, without checking the requested SAN hosts still match. A host/hostname change keeps serving a cert whose SANs omit the new host, so a non-pinning client fails hostname validation until the files are deleted by hand. Low impact while pinned workers (which skip the name check) are the primary client.

**Decision needed:** the obvious fix — regenerate on SAN mismatch — changes the pin and breaks already-distributed worker pins. Pick: warn-and-keep, error-and-instruct (tell the operator to delete the cached files), or regenerate-and-reprint the new pin.

### ACME mode does not reconcile `--tls-acme-domain` with `--addr`

**Suggested:** M5 (packaging & deployment — when ACME is exercised on a real public deployment). **Surfaced:** post-phases-6–7 review.

TLS-ALPN-01 validation requires the server reachable on `:443`, but nothing checks the listen port, so `atlas server --tls-acme-domain …` left on the default `:9090` silently never obtains a cert (every handshake fails fetching one). Only a banner note states the `:443` requirement today (`internal/cli/tls.go`, `acmeTLS`). A hard check is wrong — operators legitimately sit behind an LB/port-forward doing `:443 → :9090` — so the right move is a startup _warning_ when the listen port is not 443 and no proxy is declared.
