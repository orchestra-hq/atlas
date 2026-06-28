<!-- DRAFT launch announcement — parked here for review; not yet published, and intended for an external channel (blog / Show HN / release notes) at go-live. -->

# Point your agents at your own hardware

**Atlas is an open-source, self-hosted inference platform that makes a fleet of your own machines look like the Anthropic API. Change one environment variable and your Claude Agent SDK or Claude Code app runs on hardware you control.**

Today we're releasing Atlas v0.1.0 under Apache 2.0.

## The gap

Open-weight models have quietly closed most of the capability gap that mattered — especially for the agentic, tool-calling workloads people actually build on now. You can run a model that drives Claude Code or a Claude Agent SDK app perfectly well on a single rented GPU, a Mac Studio, or a rack you already own.

What's still painful is _serving_ them. The Claude Agent SDK and Claude Code can already be pointed at any endpoint that speaks the Anthropic Messages API — that's what `ANTHROPIC_BASE_URL` is for. But standing up "the endpoint" on your own compute is an integration project: pick an inference engine, tune it for each GPU, build an Anthropic-compatible API layer in front of it, add auth and usage tracking, and then do it again for every machine you want to add.

Atlas is that project, done once, as a product.

## What it looks like

Install the binary, start it with a model, and point your agent at it:

```sh
# install (macOS / Linux)
brew install orchestra-hq/tap/atlas
# or: curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh

# serve a model — Atlas fetches it, provisions the engine, and exposes the API
atlas up --model qwen3-0.6b
```

On first start Atlas mints an API key and prints it. Now point Claude Code at your own machine:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=<the key Atlas printed>
claude
```

That's the whole thing. No Python on the host for the slim path, no translation layer to write, no reverse proxy to configure. The same binary scales from a laptop to a multi-machine fleet — only the topology grows.

## Why we built it the way we did

The pieces of a private inference platform exist, and other projects combine them. Atlas's bet is that the _framing, the developer experience, and the curation_ are what's actually missing. A few choices fall out of that:

- **Agent-first, not model-first.** Everyone else leads with "serve models." Atlas leads with "point your agent SDK at your own infra." Drop-in compatibility with the Anthropic Messages API isn't a feature we add — it's a release gate. A conformance suite runs real Anthropic and OpenAI SDKs (and a Claude Code smoke test) on every change, and a green run blocks any release.

- **Workers dial out.** Each GPU machine runs an `atlas worker` that opens an _outbound_ connection to the control plane and keeps it alive — inference traffic flows back over that same connection. Workers need no inbound ports, no public IPs, and no SSH to operate. The security review takes minutes, and the same design makes hybrid free: a control plane in one cloud and workers anywhere — on-prem, another cloud, a Mac behind home NAT — all behind one endpoint.

- **Spot-friendly by construction.** Because workers are disposable, a spot interruption is just a heartbeat timeout; the scheduler re-places the model on whatever capacity is left. You can run an inference fleet at spot prices without building the recovery logic yourself.

- **A curated, agent-capable catalog.** Open models vary wildly in how well they actually do tool calling, and getting the chat template and engine config right is where DIY setups bleed time. Atlas ships a tested set of models that work for agents, with the correct configs out of the box.

- **Honest about scope.** Atlas documents exactly what it _doesn't_ emulate — server-side tools, batches, prompt caching — instead of half-faking them. And where it supports a genuinely hard feature, like reasoning/thinking blocks from models such as DeepSeek-R1, Qwen3, and gpt-oss, it says exactly how. We'd rather you trust the surface than be surprised by it.

## How it works

Atlas is one Go binary with two roles:

- **A control plane** (`atlas server`) — the API gateway, scheduler, model registry, auth, and usage metering. Runs anywhere; no GPU required. Its entire state is one directory you can snapshot.
- **Workers** (`atlas worker`) — one per compute machine. Each detects its hardware, launches and supervises the right inference engine for the model, and streams results back.
- **Single-node mode** (`atlas up`) — both in one process, for a laptop or a single GPU box.

Crucially, **Atlas doesn't reimplement inference.** It orchestrates the engines that already do it well — vLLM, SGLang, llama.cpp, and MLX — and puts a consistent, Anthropic- and OpenAI-compatible API in front of them. It's the layer that turns "I have some GPUs" into "we have a private, agent-ready LLM service," not another attention kernel.

## What it's not (yet)

Atlas is not a SaaS gateway that routes your prompts to someone else's cloud — it serves models on hardware you control. (There's an opt-in cloud-fallback for spillover, but it's off by default and not the point.) It's not a Kubernetes operator either; it runs great on bare metal and plain VMs, and manifests are a packaging concern, not the architecture.

This is a 0.1.0. The platform is solid — single box to multi-machine fleet, two API surfaces, auth, usage, observability from the terminal — and we're being deliberate about what comes next: a web console, and turnkey packaging (Compose / systemd / Kubernetes manifests and reference IaC) shaped by how people actually deploy rather than guessed at up front.

## Try it

- **Install:** `brew install orchestra-hq/tap/atlas` or the one-line `curl … | sh` above
- **Docs:** <https://orchestra-hq.github.io/atlas>
- **Source (Apache 2.0):** <https://github.com/orchestra-hq/atlas>

If you've ever wanted `ANTHROPIC_BASE_URL=http://your-own-box:8080` to just work, that's the whole idea. Point your agents at your own hardware and tell us what breaks.
