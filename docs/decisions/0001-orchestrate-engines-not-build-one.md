# ADR-0001: Orchestrate existing inference engines; do not build one

**Status:** accepted

## Context

Atlas needs to run open-weight models on diverse hardware (datacenter CUDA GPUs, consumer GPUs, Apple Silicon, CPU). Writing a performant inference engine is a multi-year specialist effort; the field already has heavily-funded, fast-moving leaders (vLLM, SGLang, llama.cpp, MLX), and engines also get abandoned (HF TGI archived March 2026).

## Decision

Atlas treats inference engines as **pluggable, supervised subprocesses** behind a small adapter interface (launch, health, infer, shutdown). v1 engine matrix:

- **vLLM** — CUDA GPUs, production default
- **SGLang** — CUDA GPUs, option for prefix-heavy/agent workloads
- **llama.cpp** — CPU and consumer hardware, GGUF
- **MLX** — Apple Silicon

Atlas never implements kernels, samplers, batching, or model loading. Atlas owns: engine selection, configuration, lifecycle, placement, API translation, auth, metering.

## Consequences

- We inherit engine performance improvements for free; we also inherit their breaking changes — the adapter layer and version pinning are the firewall.
- Engine processes have their own Python/CUDA dependency stacks; the worker must manage engine installation (bundled runtimes or containers — packaging decision deferred to build time).
- If an engine dies (TGI-style), we deprecate one adapter rather than the product.
