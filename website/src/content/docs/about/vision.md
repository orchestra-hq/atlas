---
title: Vision
description: Point your agents at your own hardware.
sidebar:
  order: 1
---

**Point your agents at your own hardware.** Atlas is the self-hosted inference platform that makes a
fleet of your machines look like the Anthropic (or OpenAI) API.

## The problem

Apps built on LLM APIs are easy to start and hard to control. The model runs on someone else's
infrastructure, your data leaves your network, costs scale with someone else's pricing, and
availability is someone else's SLA. Open-weight models have closed much of the capability gap —
especially for agentic, tool-calling workloads — but _serving_ them well is still an integration
project: pick an engine, tune it per GPU, build an API layer, add auth and usage tracking, repeat per
machine.

The specific gap Atlas targets is **agent SDK redirection**. The Claude Agent SDK and Claude Code can
already be pointed at any endpoint that speaks the Anthropic Messages API via `ANTHROPIC_BASE_URL`.
What's missing is a product that makes "the endpoint" trivially easy to stand up on your own compute —
single machine or fleet — without assembling vLLM + a translation layer + Kubernetes yourself.

## Who it's for

- **Developers building agents** — one GPU box or a beefy Mac, an app on the Anthropic/OpenAI SDKs,
  and the wish that `ANTHROPIC_BASE_URL=http://localhost:8080` just works.
- **Small platform / infra teams** — 2–20 GPU machines across on-prem and cloud, who want a private
  LLM-as-a-service for their org (API keys, usage visibility, model lifecycle) without a Kubernetes
  mandate.
- **Product companies offering "bring your own compute"** — ship an agent product where customers run
  inference on customer-controlled hardware: Atlas workers on their machines, one control plane.

## What Atlas is

- A **control plane** (`atlas server`) — API gateway, scheduler, model registry, auth, usage metering.
  Runs anywhere, no GPU needed.
- A **worker** (`atlas worker`) — one process per compute machine; detects hardware, launches and
  supervises the right inference engine, streams results back, and dials _out_ to the control plane.
- A **single-node mode** (`atlas up`) — both in one process, for laptops and single GPU boxes.

## What Atlas is not

- **Not an inference engine** — it orchestrates vLLM, SGLang, llama.cpp, and MLX; it doesn't write
  kernels.
- **Not a SaaS gateway to hosted providers** — it serves models on hardware you control (an opt-in
  cloud-fallback exists, but it isn't the core).
- **Not a Kubernetes operator** — it runs great on bare metal and VMs; manifests are packaging, not
  architecture.

See [Why Atlas](/atlas/about/why-atlas/) for how this plays out against the alternatives.
