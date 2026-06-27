# ADR-0009: Transport security — TLS modes and certificate pinning

## Status

accepted

## Context

[ADR-0007](0007-websocket-worker-channel.md) chose WebSocket for the worker
channel and deferred transport security to "M1 phase 7: ACME for VPS,
self-signed + pinned for private deployments." [ADR-0003](0003-control-plane-worker-split.md)'s
"join from anywhere" promise means a worker must be able to connect securely to
a server whether or not that server has a public DNS name and a CA-issued
certificate. Two realities have to be served:

1. **Public servers** — a VPS with a DNS name. Clients (the Claude Agent SDK,
   `curl`) and workers should trust it with zero manual key distribution.
2. **Private servers** — a box on a LAN, a Tailscale address, an IP with no DNS
   name and no CA willing to issue for it. There is nothing for a public CA to
   validate, but the channel still must be encrypted and authenticated against a
   man-in-the-middle.

The gateway's client API (`/v1/*`) and the worker channel (`/workers/connect`)
share one listener, so one server certificate covers both.

## Decision

`atlas server` has three mutually exclusive TLS modes, plus plaintext:

- **`--tls-acme-domain <name>`** — obtain and renew a Let's Encrypt certificate
  via `golang.org/x/crypto/acme/autocert` (TLS-ALPN-01, so the server must be
  reachable on `:443`). Certificates cache under `<state-dir>/acme`. This is the
  public-server path; clients and workers trust it through the system root store
  with no extra configuration.
- **`--tls-cert` / `--tls-key`** — serve an operator-supplied certificate
  (public-CA or otherwise).
- **`--tls-self-signed`** — generate a self-signed certificate, cache it under
  `<state-dir>/tls`, and print its **pin**. This is the private-server path.
- **default (none)** — plaintext `ws://` / `http://`, for development and
  trusted internal networks (unchanged from M0).

A **pin** is `sha256:<hex>` of the server leaf certificate's DER bytes. For the
self-signed and operator-cert modes the server prints the pin; a worker joins
with `atlas worker --join wss://… --tls-pin sha256:<hex>` (or `ATLAS_TLS_PIN`).
A pinned worker **replaces** CA-chain and hostname validation with an exact-match
check of the presented leaf against the pin (`InsecureSkipVerify` paired with a
`VerifyConnection` callback). The pin is verified, never the name — so it works
for IP-only and DNS-less servers, which is the whole point of the private path.
ACME / public-CA certs need no pin: the worker omits `--tls-pin` and the system
trust store validates them normally.

The shared pin/verify/generate primitives live in a leaf package `internal/tlsx`
imported by both `internal/cli` (server side: generate, print) and
`internal/worker` (dial side: verify).

## Consequences

- Private deployments get an encrypted, authenticated channel with no CA and no
  DNS — the operator copies one pin string from the server's startup banner to
  the worker, the same ergonomic as the join token.
- Pinning is trust-on-first-configuration, not trust-on-first-use: the operator
  moves the pin out of band, so a MITM at first dial cannot substitute its own
  certificate (unlike bare `InsecureSkipVerify`, which this never uses alone).
- A self-signed certificate is cached so its pin is stable across restarts;
  rotating it means deleting `<state-dir>/tls` and redistributing the new pin.
- ACME cannot be exercised in CI (it needs a real domain and a reachable `:443`),
  so it is covered by config-selection unit tests and manual validation; the
  `wss://` + self-signed + pin path is covered end to end per-PR, and a genuine
  two-machine run lives in the nightly/full-matrix tier.
- The self-signed certificate is a non-CA leaf (`IsCA: false`, no
  `KeyUsageCertSign`): pinning trusts its exact leaf bytes, so the cert never needs
  to sign anything, and keeping the on-disk private key out of CA scope limits the
  blast radius if it leaks. Private trust is therefore via the pin, not by
  distributing the cert as a CA root. (An earlier draft minted it `IsCA`; tightened
  to a leaf in [PR #34](https://github.com/orchestra-hq/atlas/pull/34) after a security review.)
