# Vision

## One-liner

**Point your agents at your own hardware.** Atlas is the self-hosted inference platform that makes a fleet of your machines look like the Anthropic (or OpenAI) API.

## The problem

Apps built on LLM APIs are easy to start and hard to control. The model runs on someone else's infrastructure, the data leaves your network, the costs scale with someone else's pricing, and availability is someone else's SLA. Open-weight models have closed much of the capability gap — especially for tool-calling/agentic workloads — but actually *serving* them well is still an integration project: pick an engine, tune it per GPU, build an API layer, add auth and usage tracking, repeat per machine.

Specific gap we target: **agent SDK redirection**. The Claude Agent SDK and Claude Code can already be pointed at any endpoint that speaks the Anthropic Messages API via `ANTHROPIC_BASE_URL`. What's missing is a product that makes "the endpoint" trivially easy to stand up on your own compute — single machine or fleet — without assembling vLLM + LiteLLM + Kubernetes yourself.

## Who it's for

1. **Developers building agents** (primary, drives adoption). They have one GPU box or a beefy Mac, an app built on the Anthropic/OpenAI SDKs, and want `ANTHROPIC_BASE_URL=http://localhost:9090` to just work. They judge us against Ollama's DX.
2. **Small platform/infra teams** (primary, drives depth). They have 2–20 GPU machines across on-prem and cloud and want a private LLM-as-a-service for their org: API keys, usage visibility, model lifecycle, no Kubernetes mandate. They judge us against GPUStack and DIY vLLM.
3. **Product companies offering "bring your own compute"** (strategic, this is *us* — we will build tools on top). They ship an agent product and want customers to run inference on customer-controlled hardware. Atlas workers on the customer's machines + a control plane (theirs or the vendor's) is the deployment story.

## What Atlas is

- A **control plane** (`atlas server`): API gateway, scheduler, model registry, auth, usage metering, web console. Runs anywhere, no GPU needed.
- A **worker** (`atlas worker`): one process per compute machine. Detects hardware, launches and supervises the right inference engine for the model and the hardware, streams results back. Dials *out* to the control plane.
- A **single-node mode** (`atlas up`): both in one process, Ollama-style, for laptops and single GPU boxes.

## What Atlas is not

- **Not an inference engine.** We orchestrate vLLM, SGLang, llama.cpp, and MLX; we do not write kernels (ADR-0001).
- **Not a SaaS gateway to hosted providers.** LiteLLM/OpenRouter route to other people's clouds; Atlas serves models on hardware you control. (A passthrough/fallback-to-cloud feature may come later, but it is not the core.)
- **Not a Kubernetes operator.** It must run great on bare metal and VMs. K8s deployment manifests are a packaging concern, not the architecture.
- **Not a model-sharding research project.** Splitting one model across many small devices (exo's territory) is out of scope initially; we place whole models (or engine-managed tensor-parallel groups) on capable machines.

## How we win (honest version)

The components of this product exist and several projects (GPUStack most directly) already combine them. Our differentiation is deliberately not a novel systems idea. It is:

1. **Agent-first framing.** Everyone else leads with "serve models"; we lead with "point your agent SDK at your own infra" — including model-alias mapping so SDK defaults (e.g. `claude-sonnet-*`) resolve to your deployed models, and first-class docs/recipes for Claude Agent SDK, Claude Code, and OpenAI Agents SDK.
2. **DX as the product.** One static binary, one command to a working endpoint, one command to join a machine to the fleet. Measured in minutes-to-first-token.
3. **Opinionated model curation for agents.** A tested catalog of open models that actually do tool calling well, with correct chat templates and engine configs out of the box — this is where DIY setups bleed time.
4. **Marketing and ecosystem.** Better docs, better positioning, and tools built on top (separate projects) that make Atlas the default substrate.

## Success criteria for v1

- A developer can go from `curl -fsSL get.atlas.dev | sh` to Claude Code running against a local open model in under 10 minutes.
- A team can join three GPU machines to one control plane and serve two models behind one authenticated endpoint without touching Kubernetes.
- An existing app using the Anthropic SDK works against Atlas by changing only `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, and model name (or relying on alias mapping).
