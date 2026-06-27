---
title: TLS
description: Secure the endpoint with ACME, an operator certificate, or self-signed + pin.
sidebar:
  order: 4
---

`atlas server` serves plaintext (`ws://`/`http://`) by default — fine behind an SSH tunnel or on a
trusted network. For an exposed endpoint, pick one TLS mode.

## Let's Encrypt (public DNS name)

```sh
atlas server --tls-acme-domain atlas.yourco.com
```

Obtains and renews a Let's Encrypt certificate. The server must be reachable on `:443` for the
TLS-ALPN-01 challenge. Clients and workers trust it through the system root store — no pin needed.

## Operator-supplied certificate

```sh
atlas server --tls-cert /path/cert.pem --tls-key /path/key.pem
```

Use your own certificate (e.g. from an internal CA or an ALB doing TLS termination upstream).

## Self-signed + pin (private fleet, no DNS/CA)

```sh
atlas server --tls-self-signed
```

Generates a certificate cached in the state dir. The startup banner prints a `sha256:<hex>` **pin**;
each worker joins over `wss://` with `--tls-pin <pin>` (or `ATLAS_TLS_PIN`), which authenticates the
exact certificate instead of a CA chain or hostname. The pin is stable across restarts (the material
is cached); rotate by deleting the state dir's `tls/` directory and redistributing the new pin.

Behind an ALB/Route53, terminate TLS at the load balancer (ACM cert) and let the server speak
plaintext inside the VPC — see [Cloud fleet](/atlas/deploy/cloud-fleet/).
