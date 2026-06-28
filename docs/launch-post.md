<!-- DRAFT launch announcement — parked here for review; not yet published, and intended for an external channel (blog / Show HN / release notes) at go-live. -->

# Point your agents at your own hardware

**Atlas is an open-source, self-hosted inference platform that makes a fleet of your own machines look like the Anthropic API. Change one environment variable and your Claude Agent SDK or Claude Code app runs on hardware you control.**

Today we're releasing Atlas v0.1.0 under Apache 2.0.

## Why we built this

Over the last several months we kept having the same conversation. We'd be talking to a developer building an agent, or a small platform team running a couple of GPU boxes, or a company shipping an AI product to security-conscious customers — and the shape of the problem was always the same.

They liked building on the Claude Agent SDK and the Anthropic API. They didn't love that every request left their network, that the bill scaled with someone else's pricing, or that their roadmap now depended on someone else's rate limits and SLA. And they'd noticed the thing everyone has noticed: open-weight models have gotten genuinely good — good enough to drive real agentic, tool-calling work, not just demos.

So the obvious question kept coming up: _why can't I just point this at my own hardware?_

The frustrating part is that you almost can. The Claude Agent SDK and Claude Code already let you redirect to any endpoint that speaks the Anthropic Messages API — that's exactly what `ANTHROPIC_BASE_URL` is for. The missing piece was never the SDK. It was that _standing up the endpoint_ on your own compute is a project: choose an inference engine, tune it for each GPU, build and maintain an Anthropic-compatible API layer in front of it, add auth and usage tracking, and then repeat the whole thing for every machine you want to add. Everyone we spoke to had either started building that glue, abandoned building that glue, or was paying someone else's cloud to avoid building that glue.

Atlas is that glue, done once, as a product you install. The goal is narrow and specific: make "the endpoint" trivial to stand up on machines you own — one laptop or a fleet — so that pointing your agents at your own hardware is a one-line change and nothing else.

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

## Models that work out of the box

One of the things people burn the most time on with a DIY setup isn't the API layer — it's getting a model to actually behave: the right chat template, the right tool-call parser, sane sampling defaults, reasoning toggles that don't leak `<think>` blocks into normal replies. A model that scores well on a leaderboard can still be useless for agents if its tool-calling config is wrong.

So Atlas ships a curated, agent-tested catalog. Each entry comes with the engine, the parser flags, and the defaults that make tool calling and reasoning work correctly — you just name the model. The starter catalog spans all four engines and three rough capability tiers:

| Where you run it     | Engine       | Models in the starter catalog                                       |
| -------------------- | ------------ | ------------------------------------------------------------------- |
| **Laptop / CPU box** | llama.cpp    | Qwen2.5-1.5B-Instruct, Qwen3-0.6B (reasoning)                       |
| **NVIDIA GPU**       | vLLM, SGLang | Qwen3-8B (reasoning), Qwen3.5-35B-A3B, GLM-5.1, Qwen2.5-7B-Instruct |
| **Apple Silicon**    | MLX (Metal)  | Qwen2.5-1.5B-Instruct, Qwen2.5-7B-Instruct                          |
| **Coding**           | llama.cpp    | Gemma-4-12B coder finetune (reasoning, 256K context, Apache-2.0)    |

The small llama.cpp tier cold-boots on a plain CPU — it's what runs in our own CI on every change — and is great for development, evals, and offline work. The capable GPU tiers are sized to drive Claude Code for real.

Agents need more than chat, so the catalog also includes the rest of the stack:

- **Embeddings** — `nomic-embed-text-v1.5`, served on the OpenAI `/v1/embeddings` shape.
- **Reranking** — `bge-reranker-v2-m3`, on the de-facto Cohere `/v1/rerank` shape.

A couple of conveniences fall out of the catalog. **Model aliases** map the Claude names your code already uses — `claude-sonnet-*`, `claude-opus-*`, `claude-haiku-*` — onto whatever you've actually deployed, so you don't have to touch model strings scattered through an app. And when the catalog isn't enough, **bring your own**: point Atlas at any Hugging Face repo id or a local weights file and it'll serve it best-effort. The catalog is the curated fast path, not a fence.

## Why we built it the way we did

The pieces of a private inference platform exist, and other projects combine them. Atlas's bet is that the _framing, the developer experience, and the curation_ are what's actually missing. A few choices fall out of that:

- **Agent-first, not model-first.** Everyone else leads with "serve models." Atlas leads with "point your agent SDK at your own infra." Drop-in compatibility with the Anthropic Messages API isn't a feature we add — it's a release gate. A conformance suite runs real Anthropic and OpenAI SDKs (and a Claude Code smoke test) on every change, and a green run blocks any release.

- **Workers dial out.** Each GPU machine runs an `atlas worker` that opens an _outbound_ connection to the control plane and keeps it alive — inference traffic flows back over that same connection. Workers need no inbound ports, no public IPs, and no SSH to operate. The security review takes minutes, and the same design makes hybrid free: a control plane in one cloud and workers anywhere — on-prem, another cloud, a Mac behind home NAT — all behind one endpoint.

- **Spot-friendly by construction.** Because workers are disposable, a spot interruption is just a heartbeat timeout; the scheduler re-places the model on whatever capacity is left. You can run an inference fleet at spot prices without building the recovery logic yourself.

- **Honest about scope.** Atlas documents exactly what it _doesn't_ emulate — server-side tools, batches, prompt caching — instead of half-faking them. And where it supports a genuinely hard feature, like reasoning/thinking blocks from models such as DeepSeek-R1, Qwen3, and gpt-oss, it says exactly how. We'd rather you trust the surface than be surprised by it.

## How it works

Atlas is one Go binary with two roles:

- **A control plane** (`atlas server`) — the API gateway, scheduler, model registry, auth, and usage metering. Runs anywhere; no GPU required. Its entire state is one directory you can snapshot.
- **Workers** (`atlas worker`) — one per compute machine. Each detects its hardware, launches and supervises the right inference engine for the model, and streams results back.
- **Single-node mode** (`atlas up`) — both in one process, for a laptop or a single GPU box.

Crucially, **Atlas doesn't reimplement inference.** It orchestrates the engines that already do it well — vLLM, SGLang, llama.cpp, and MLX — and puts a consistent, Anthropic- and OpenAI-compatible API in front of them. It's the layer that turns "I have some GPUs" into "we have a private, agent-ready LLM service," not another attention kernel.

## Who it's for

The conversations that led here clustered into three groups, and Atlas is built for all three:

- **Developers building agents.** One GPU box, a beefy Mac, or even a laptop, an app on the Anthropic or OpenAI SDKs, and the wish that `ANTHROPIC_BASE_URL=http://localhost:8080` just works. Atlas is the shortest path from "I have a model" to "my agent is running on it."

- **Small platform and infra teams.** Two to twenty GPU machines spread across on-prem and cloud, who want a private LLM-as-a-service for their org — API keys, usage visibility, model lifecycle — without adopting a Kubernetes mandate to get it.

- **Product companies offering "bring your own compute."** Ship an agent product where customers run inference on their _own_ hardware: Atlas workers on the customer's machines dialing out to one control plane, prompts and weights never leaving their environment. The dial-out design makes this a configuration, not a re-architecture.

## Where it's going

This is a 0.1.0, and the foundation is deliberately solid: single box to multi-machine fleet, Anthropic- and OpenAI-compatible APIs, per-key auth, durable usage metering, backpressure, embeddings and reranking, and full fleet observability from the terminal. We shipped that first because it's the part you can't fake.

What's next, roughly in order:

- **A web console.** The terminal tools (`atlas status`, `atlas top`) already let you operate a fleet; a visual console is the natural next surface for people who'd rather click than SSH.
- **Turnkey packaging and reference IaC.** Compose files, systemd units, Kubernetes manifests, and a reference cloud module — shaped by how people actually deploy Atlas rather than guessed at up front. We'd rather build these against real demand than ship manifests nobody asked for.
- **Further out:** a high-availability control plane for teams who outgrow a single always-on box, and deeper support for the "bring your own compute" product pattern.

We're building this in the open, and the roadmap is genuinely shaped by what we hear. If your use case isn't quite covered yet, tell us — that's how the last six months of conversations turned into this release, and it's how the next ones will turn into the next.

## What it's not

To set expectations honestly: Atlas is not a SaaS gateway that routes your prompts to someone else's cloud — it serves models on hardware you control. (There's an opt-in cloud-fallback for spillover, but it's off by default and not the point.) And it's not a Kubernetes operator; it runs great on bare metal and plain VMs, and manifests are a packaging concern, not the architecture.

## Try it

- **Install:** `brew install orchestra-hq/tap/atlas` or the one-line `curl … | sh` above
- **Docs:** <https://orchestra-hq.github.io/atlas>
- **Source (Apache 2.0):** <https://github.com/orchestra-hq/atlas>

If you've ever wanted `ANTHROPIC_BASE_URL=http://your-own-box:8080` to just work, that's the whole idea. Point your agents at your own hardware and tell us what breaks.
