# Usage scenarios: which path fits you

Atlas is one binary that scales from a laptop to a fleet. This maps common situations to the shortest path that works, so you don't read the whole docs tree to get started. Every path ends the same way: an `ANTHROPIC_BASE_URL` (and OpenAI-compatible base URL) you point existing tools at.

| You have…                      | Engine    | Path                                  |
| ------------------------------ | --------- | ------------------------------------- |
| A laptop / dev box (no GPU)    | llama.cpp | [Local](#local-laptop-or-dev-box)     |
| One rented cloud GPU           | vLLM      | [Single cloud GPU](#single-cloud-gpu) |
| Several machines, one endpoint | either    | [Fleet](#fleet-several-machines) (M1) |

## Local (laptop or dev box)

The hero path: run an open model and drive Claude Code against it, no GPU needed.

```bash
atlas up --model qwen3-0.6b           # pulls from the starter catalog, serves on :8080
# in another shell:
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<printed key> claude
```

Prefer containers? The slim image does the same — see [docs/docker.md](docker.md). One-shot, no daemon? `atlas run qwen3-0.6b "your prompt"`.

This is great for development, evals, and offline work. A small model drives Claude Code only intermittently; for reliable agentic use, move to a GPU.

## Single cloud GPU

The credibility path: a capable model on one rented ~24GB GPU, vLLM under the hood, your tools pointed at it. Two recipes (full detail in [examples/serve/](../../examples/serve/README.md)):

- **SkyPilot one-command** — `sky launch examples/serve/atlas-serve.sky.yaml …` brings up the cheapest available GPU across your clouds and serves a model. Best when you want it picked for you.
- **Single box + SSH tunnel** — already have a GPU box (any cloud, or your own)? Install the binary or the [CUDA image](docker.md), serve bound to localhost, and reach it over an SSH tunnel. Zero extra tooling, nothing exposed.

Either way:

```bash
ANTHROPIC_BASE_URL=http://<your-endpoint> ANTHROPIC_API_KEY=<your key> claude
```

## Fleet (several machines)

> Shipped in **M1** ([roadmap](../roadmap.md)) — `atlas server` + `atlas worker --join`.

Run `atlas server` on a small always-on box, then `atlas worker --join <token>` on each GPU machine (a 4090 under your desk, a cloud box, a customer's host). Workers **dial out** to the server (ADR-0003) — no inbound ports on the compute boxes — and one authenticated endpoint serves models across all of them. The same binary, the same API surface; only the topology grows.

## See also

- [docs/docker.md](docker.md) — the container images (slim + CUDA)
- [examples/serve/](../../examples/serve/README.md) — the cloud-GPU serve recipes
- [docs/api-surface.md](api-surface.md) — exactly what the endpoint exposes
- [docs/deployment-aws.md](deployment-aws.md) — reference AWS topology and the requirements it imposes
