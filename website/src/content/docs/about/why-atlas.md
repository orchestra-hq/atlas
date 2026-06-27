---
title: Why Atlas
description: The differentiators — agent-first, spot-friendly, zero-inbound, hybrid, honest scope.
sidebar:
  order: 2
---

The pieces of a private inference platform exist, and other projects combine them. Atlas's
differentiation is deliberately about framing, DX, and curation rather than a novel systems idea.

## What sets it apart

- **Agent-first compatibility.** Everyone else leads with "serve models"; Atlas leads with "point your
  agent SDK at your own infra." Change `ANTHROPIC_BASE_URL` and your Claude Agent SDK / Claude Code app
  runs on your hardware — drop-in is a release gate, not a feature. Model aliases map SDK defaults
  (e.g. `claude-sonnet-*`) to your deployed models.
- **Minutes to first token.** One static Go binary, one command to a working endpoint, one command to
  join a machine to the fleet. No Python on the host for the slim path.
- **Spot-friendly GPU fleets.** Workers are disposable — a spot interruption is just a heartbeat
  timeout, and the scheduler re-places the model on remaining capacity. Run inference at spot prices.
- **Zero-inbound-ports security.** GPU workers dial out, so they need no inbound rules, no public IPs,
  no SSH to operate. The security review takes minutes.
- **Hybrid for free.** Because workers dial out, the control plane can be in one place and workers
  anywhere — on-prem, multiple clouds, a Mac behind NAT — all behind one endpoint.
- **Curated agent-capable catalog.** A tested set of open models that actually do tool calling well,
  with correct chat templates and engine configs out of the box — where DIY setups bleed time.
- **Data sovereignty.** Prompts, outputs, and weights never leave hardware you control (unless you opt
  into cloud-fallback).
- **Honest API scope.** Atlas documents exactly what it doesn't emulate (server-side tools, batches,
  prompt caching) rather than half-faking it — and exactly how it _does_ support hard features like
  reasoning/thinking blocks. See [API compatibility](/atlas/reference/api-compatibility/).
- **No Kubernetes mandate.** Great on bare metal and plain VMs; manifests are packaging, not
  architecture.

## How it compares

- **vs Ollama** — Ollama-grade local DX, but for your whole fleet, with an agent-native API.
- **vs DIY (vLLM + a translation layer + K8s)** — the same capability without the assembly: one binary,
  Anthropic-native API, an agent-first catalog.
- **vs cloud API routers** — routers send your prompts to other people's clouds; Atlas runs the models
  on yours.
