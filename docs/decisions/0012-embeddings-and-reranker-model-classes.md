# ADR-0012: Embeddings and reranker model classes

## Status

proposed

## Context

Atlas has served one model class to date — chat/completion models behind `/v1/messages` and `/v1/chat/completions`. A self-hosted RAG or agent stack needs two more first-class classes on the same fleet: **embedding** models (text → vector, for retrieval) and **reranker** models (query + documents → relevance scores, for precision). Today an operator running Atlas for chat has to stand up a second, unrelated service for embeddings and rerank, which defeats the "one endpoint, your hardware" promise for exactly the stacks Atlas is courting.

The engines Atlas already wraps support these tasks: vLLM serves embeddings (`--task embed`) and scoring/rerank, SGLang likewise, and llama.cpp exposes `/embedding`. Per [ADR-0001](0001-orchestrate-engines-not-build-one.md), Atlas must wrap those capabilities, not compute embeddings or scores itself.

Surface constraints:

- The Anthropic Messages API ([ADR-0002](0002-anthropic-api-first.md)) has **no** embeddings or rerank endpoint, so there is no first-class surface to mirror — embeddings/rerank are an OpenAI-compat-and-native concern, and adding them does not touch the Anthropic drop-in promise.
- `POST /v1/embeddings` is the OpenAI-standard embeddings shape ([api-surface.md](../api-surface.md) already lists it as "later"). Rerank has **no** OpenAI standard; the de-facto convention is Cohere's `/v1/rerank` (query + documents + top_n → ordered results), which vLLM and others already emulate.

## Decision

1. **Introduce a model `class` field: `chat` | `embedding` | `reranker`, defaulting to `chat`.** It is recorded on catalog entries and on the resolved model, and threads from the catalog through the scheduler to the gateway. Existing chat models and raw specs default to `chat`, so nothing about the current surface changes.

2. **The gateway routes by class and rejects mismatches cleanly.** An embeddings request resolves only to an `embedding` model and a rerank request only to a `reranker`; sending a request to a model of the wrong class returns a well-formed Anthropic/OpenAI-shaped error (not a 5xx), the same way an unknown model does. Chat endpoints stay restricted to `chat` models.

3. **Serve embeddings on `POST /v1/embeddings` (OpenAI shape) and rerank on a native `POST /v1/rerank` (Cohere shape).** Embeddings reuse the OpenAI surface because that is where the standard and the SDK ecosystem live; rerank gets a native Atlas endpoint following the Cohere convention because there is no OpenAI equivalent to mirror. Both are documented in [api-surface.md](../api-surface.md).

4. **Wrap engine capabilities through the shared translation layer.** The shared `internal/engines/openaichat` client gains sibling embedding/rerank calls (or a parallel minimal client) so vLLM, SGLang, and MLX reuse one translation path, exactly as they share the chat translation. llama.cpp's `/embedding` is wrapped for the embedding class. Atlas computes nothing — it forwards to the engine and shapes the response.

5. **Class-aware scheduling reuses the existing placement policy.** Embedding and reranker models are placed by the same VRAM-fit logic as chat models ([architecture.md](../architecture.md) scheduler); they differ only in that they do not stream and have no tool loop, so the admission/affinity layer treats them as single-shot requests.

6. **The starter catalog gains at least one embedding model and one reranker, with pinned digests.** The M0 pinning rule holds ([internal/catalog](../m0-build-plan.md)): a weights entry without a verified digest is rejected.

## Consequences

- A single Atlas fleet serves chat, embeddings, and rerank behind one authenticated endpoint, so a self-hosted RAG/agent stack no longer needs a second service — the headline ecosystem expansion of M3.
- The `class` field is a small, backward-compatible addition: everything existing is `chat` by default, and class routing is an additional resolution check, not a rewrite of the request path.
- Embeddings/rerank are OpenAI/native-only by design; the Anthropic first-class surface and its drop-in promise are untouched, so this phase carries no compat risk for Claude Code / the Agent SDK.
- Rerank follows the Cohere convention rather than an OpenAI one because none exists; if an embeddings/rerank standard later emerges on the Anthropic or OpenAI surface, the `class` abstraction localizes the change to the endpoint layer.
- count-token / context-window assertion differs per class (an embedding request has no `max_tokens`); the gateway's pre-dispatch checks branch on class, reusing the per-engine token-count capability already threaded for chat.
- Each new class is only as proven as the catalog rows feeding it; embedding/reranker conformance (G20) runs on llama.cpp per-PR and on the GPU engines nightly, the same tiering as the chat engines.
