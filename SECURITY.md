# Security policy

## Reporting a vulnerability

**Please do not report security vulnerabilities in public issues or pull requests.**

Report them privately through GitHub's private vulnerability reporting: open the
[Security tab](https://github.com/orchestra-hq/atlas/security/advisories/new) on this repository and
file a draft advisory. Only the maintainers can see it.

Please include enough detail to reproduce the issue — the affected version or commit, the
configuration (engine, single-node or fleet, TLS mode), and what an attacker gains. A proof of
concept helps but is not required.

We aim to acknowledge a report within a few working days. Atlas is a young project maintained by a
small team, so please treat those timelines as best-effort rather than a contractual SLA.

## Supported versions

Atlas is pre-1.0. Security fixes land on `main` and go out in the next release; there are no
long-term support branches yet, so **the latest release is the only supported version**.

## Scope

Atlas is a control plane plus per-machine workers that orchestrate third-party inference engines
(vLLM, SGLang, llama.cpp, MLX). In scope for this policy:

- The Atlas control plane and its HTTP surface — `/v1/messages`, the OpenAI-compatible endpoints,
  and the admin/control endpoints.
- API-key handling, the audit log, and usage metering.
- The worker channel: the outbound WebSocket, its TLS and certificate pinning, and request/response
  proxying over it.
- The model store, catalog digest verification, and runtime provisioning.
- Release integrity: `install.sh`, the published archives, and their checksum and cosign signatures.

Out of scope — please report these upstream:

- Vulnerabilities in the inference engines themselves, or in the models they serve.
- Model behaviour, including prompt injection against a model Atlas is serving and any content it
  generates.

## Operating Atlas safely

Two defaults worth knowing when you deploy:

- On first run both `atlas up` and `atlas server` mint a **default full-access admin key**. Treat
  that key as a root credential: issue scoped per-client keys with `atlas keys create` for anything
  that isn't you at a terminal.
- `atlas up` listens on `127.0.0.1:8080` (local only), but `atlas server` defaults to
  `0.0.0.0:9090` — every interface. Before running `atlas server` on a machine with a public or
  shared network, enable TLS and narrow the bind address with `--addr`; see the
  [operate guides](https://orchestra-hq.github.io/atlas).
- Workers dial out to the control plane and never listen for inbound connections
  ([ADR-0003](docs/internal/decisions/0003-control-plane-worker-split.md)). If a deployment appears
  to require an inbound rule to a worker, something is misconfigured — that is worth a report.
