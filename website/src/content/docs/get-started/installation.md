---
title: Installation
description: Install the atlas binary via Homebrew, the one-line installer, or the container image.
sidebar:
  order: 1
---

Atlas ships as a single static binary (macOS and Linux, amd64 and arm64), plus container images.
Pick whichever fits.

## Homebrew (macOS / Linuxbrew)

```sh
brew install orchestra-hq/tap/atlas
```

## One-line installer

The installer detects your OS/arch, downloads the matching release, and verifies its checksum and
cosign signature before installing:

```sh
curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh
```

It is non-interactive and scriptable. Useful environment variables:

| Variable            | Effect                                                            |
| ------------------- | ---------------------------------------------------------------- |
| `ATLAS_VERSION`     | Install a specific tag (e.g. `v0.1.0`) instead of the latest     |
| `ATLAS_INSTALL_DIR` | Install location (default `/usr/local/bin`, or `~/.local/bin`)   |

## Container image

```sh
# CPU (llama.cpp): the general default
docker run --rm -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:slim up --model qwen3-0.6b --addr 0.0.0.0:8080

# GPU (vLLM): requires the NVIDIA Container Toolkit
docker run --rm --gpus all -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:cuda up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080
```

See the [Docker guide](/atlas/deploy/) for image variants, state, and keys.

## Verify

```sh
atlas version
```

Then head to the [quickstart](/atlas/get-started/quickstart/).
