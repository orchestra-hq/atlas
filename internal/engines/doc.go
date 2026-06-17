// Package engines holds the adapters that drive inference engines as
// subprocesses (ADR-0001), one subpackage per engine: llamacpp and vllm
// in M0, sglang and mlx later. Adapters consume each engine's
// OpenAI-compat/native endpoint; the gateway owns all Anthropic semantics
// (build-time decision 1 in docs/m0-build-plan.md).
//
// Both M0 engines speak the same OpenAI chat-completions wire, so the
// core⇄OpenAI translation and the generation client live once in the shared
// subpackage openaichat; each adapter embeds it and adds only its
// engine-specific endpoints (token counting, context window).
//
// Populated from phase 2 (llamacpp) and phase 8 (vllm, openaichat) of
// docs/m0-build-plan.md.
package engines
