# Open questions

Decisions that need Will's call (or at least sign-off). Each has a recommendation so the default path is unblocked.

## 1. Name

"Atlas" is a placeholder from the directory name and almost certainly collides (multiple Atlas-named dev tools exist, incl. MongoDB Atlas). Need a brandable, npm/crates/domain-available name before anything public. **Recommendation:** keep `atlas` internally, run a naming exercise before first public push.

## 2. Language: Go (ADR-0004 is `proposed`)

Go vs Python for the platform binary. ADR-0004 argues Go (single-binary DX is core differentiation; engines are subprocesses anyway; Ollama precedent). Python's case: ML contributor pool, GPUStack precedent, faster glue iteration. **Recommendation: Go.** Needs sign-off to flip ADR-0004 to `accepted`.

## 3. License

**Recommendation: Apache 2.0** (patent grant, infra-adoption norm, matches vLLM/GPUStack). Alternative MIT (simpler, Ollama's choice). If a hosted/open-core business is intended later, this choice has consequences — flag now, decide before first release.

## 4. M0 engine pair

Proposed: llama.cpp (universal dev experience) + vLLM (CUDA credibility). Alternative: ship only llama.cpp in M0 for speed, vLLM in M0.5. **Recommendation: both in M0** — the agent-redirection demo is unconvincing on small quantized models alone; vLLM on a real GPU is the demo that lands.

## 5. How do workers get engine runtimes?

vLLM/SGLang need a Python+CUDA stack on the worker. Options: (a) require Docker/Podman and run engines as containers; (b) worker-managed venvs (uv) per engine; (c) both, container preferred. **Recommendation: (c)** — containers where available, managed venv fallback for bare metal. Decide at M0 build time; affects ADR-0004 consequences.

## 6. Scope guard: multi-machine model sharding

exo-style splitting of one model across several small machines is explicitly out of scope (vision.md). Confirm — it's a frequent community ask and a scope-creep magnet. **Recommendation: confirm out of scope; revisit only with overwhelming demand.**

## 7. Open-core / monetization posture

Not needed now, but the architecture has natural seams (hosted control plane, enterprise console features). Worth a deliberate decision before the project has outside contributors with expectations. **Recommendation: defer, but document intent before first external contribution.**

## 8. Relationship to the existing app

Will's current app uses the Claude Agent SDK against Anthropic. The stated goal is to eventually point its LLM requests at Atlas. Question: does the app's needs list (specific endpoints/features it exercises) become the v1 conformance checklist? **Recommendation: yes — extract the app's actual API usage (which endpoints, streaming, tool use patterns) and make it the M0 acceptance test.** Needs a pointer to that codebase.
