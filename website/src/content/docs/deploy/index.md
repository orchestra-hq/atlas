---
title: Deploy
description: Run Atlas on a laptop, a single GPU box, Docker, or a cloud fleet — one binary, one endpoint.
---

Atlas is one binary that scales from a laptop to a fleet. Every path ends the same way: an
`ANTHROPIC_BASE_URL` (and an OpenAI-compatible base URL) your tools point at. Pick the path that
matches what you have.

| You have…                      | Path                                                        |
| ------------------------------ | ---------------------------------------------------------- |
| A laptop / dev box (no GPU)    | [Laptop](/atlas/deploy/laptop/)                            |
| Containers                     | [Docker](/atlas/deploy/docker/)                            |
| One rented cloud GPU           | [Single GPU box](/atlas/deploy/single-gpu-box/)           |
| Several machines, one endpoint | [Cloud fleet](/atlas/deploy/cloud-fleet/)                 |

All paths use what Atlas ships today — the binary, the [installer](/atlas/get-started/installation/),
and the container images.

:::note[Packaging recipes are a later milestone]
First-party packaging — a Docker Compose file, systemd units, Kubernetes manifests, and reference
Terraform — is deferred to a demand-driven milestone (M7), so it's pulled by how operators actually
deploy rather than guessed ahead of time. The paths here use the binary, the installer, the container
images, and a reference cloud topology you can adapt.
:::
