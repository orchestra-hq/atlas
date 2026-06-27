---
title: Single GPU box
description: Serve a capable model on one rented GPU with vLLM, reached over an SSH tunnel.
sidebar:
  order: 3
---

The credibility path: a capable model on one ~24GB GPU, vLLM under the hood, your tools pointed at it.
Two recipes.

## Single box + SSH tunnel (zero extra tooling)

Already have a GPU box (any cloud, or your own)? Install the binary or use the
[CUDA image](/atlas/deploy/docker/), serve bound to localhost, and reach it over an SSH tunnel —
nothing exposed to the internet.

```sh
# on the GPU box
curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh
atlas up --engine vllm --model Qwen/Qwen3-8B

# from your laptop: forward the port over SSH, then point a client at localhost
ssh -N -L 8080:localhost:8080 user@gpu-box &
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<key from box logs> claude
```

## SkyPilot one-command

If you'd rather have the cheapest available GPU picked for you across clouds, the SkyPilot recipe
brings up a ~24GB GPU and serves a model in one command. It's the labelled "easy button"; the binary
never depends on SkyPilot. See the [serve recipes](https://github.com/orchestra-hq/atlas/tree/main/examples/serve)
in the repo.

## Exposing the endpoint

The single-box recipe keeps the gateway on localhost behind an SSH tunnel — the safest default. To
expose it directly, terminate TLS (see [TLS](/atlas/operate/tls/)) and keep the security group tight.
The gateway always requires an API key, so the endpoint is never open even with the port published.

For more than one GPU machine behind one endpoint, see [Cloud fleet](/atlas/deploy/cloud-fleet/).
