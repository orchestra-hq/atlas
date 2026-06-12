// Package engines holds the adapters that drive inference engines as
// subprocesses (ADR-0001), one subpackage per engine: llamacpp and vllm
// in M0, sglang and mlx later. Adapters consume each engine's
// OpenAI-compat/native endpoint; the gateway owns all Anthropic semantics
// (build-time decision 1 in docs/m0-build-plan.md).
//
// Populated from phase 2 of docs/m0-build-plan.md.
package engines
