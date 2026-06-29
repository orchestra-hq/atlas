# Adding support for a model family

Atlas serves _any_ Hugging Face model: it reads the model's published metadata and
auto-configures the engine from the model's **family** ([ADR-0015](decisions/0015-bring-any-model-auto-configuration.md),
[m8-build-plan.md](m8-build-plan.md)). Most settings — engine, context window, chat
template, sampling — are derived from the metadata. The one cluster that is **not**
derivable is the per-family agent config: which **tool-call parser** and **reasoning
parser** an engine needs to make tool-calling and `thinking` work. That knowledge lives
in a single in-code map, [`internal/modelmeta/family.go`](../../internal/modelmeta/family.go).

When you serve a model Atlas can load and fit but whose family is not in that map, it is
served as **plain chat** with a warning (or refused under `--require-verified`):

```text
warning: serving org/foo-7b as plain chat — no tested tool-call/reasoning config for
FooForCausalLM (foo), so tool-calling and reasoning may misbehave. To add agent support,
add the family to internal/modelmeta/family.go and open a PR
```

This page is the "open a PR" the message points at. Adding a family is a small,
self-contained change: an entry in the family map, and a conformance case that proves it.

## The one-file change

Everything is in [`internal/modelmeta/family.go`](../../internal/modelmeta/family.go).

1. **Map the model's type/architecture to a family token.** `Classify` reduces a model's
   `model_type` (HF `config.json`) or `general.architecture` (GGUF), with the
   `architectures[0]` class name as a fallback, to a canonical token via
   `normalizeFamily` (prefix match, case-insensitive). If your family's token isn't
   produced yet, add a `case` to `normalizeFamily`:

   ```go
   case strings.HasPrefix(s, "mistral"):
       return "mistral"
   ```

   This matches both `model_type: "mistral"` and `architectures: ["MistralForCausalLM"]`.

2. **Add a `Family` entry to `families`.** Set the token, whether the family reasons
   natively (this gates the `enable_thinking` kwarg, engine-agnostically), and the
   per-engine parser names:

   ```go
   {
       Name:      "mistral",
       Reasoning: false,
       parsers: map[string]engineParsers{
           "vllm":   {toolCall: "mistral"},
           "sglang": {toolCall: "mistral"},
       },
   },
   ```

   - `parsers` is keyed by the **bare engine name** (`"vllm"` / `"sglang"`). The values
     are that engine's own parser identifiers, which **differ across engines** for the
     same family (e.g. Qwen2.5 is `hermes` on vLLM but `qwen25` on SGLang) — take them
     verbatim from the engine's docs, not by analogy.
   - `EngineArgs` renders the flags: vLLM also gets `--enable-auto-tool-choice` (it
     rejects `--tool-call-parser` without it). Set `reasoning:` on `engineParsers` only
     when the family emits a reasoning channel that engine can parse.
   - **Omit an engine you have not verified.** An absent engine entry serves with no
     parser flags (template-driven) rather than a guessed one. **llama.cpp and MLX are
     always template-driven** and take no `parsers` entry — they apply the model's own
     chat template.

3. **Check the architecture is loadable.** A family is only reachable if the engine can
   load the architecture in the first place. The allowlist is
   [`internal/modelmeta/arch.go`](../../internal/modelmeta/arch.go) (`transformersArchs`
   for vLLM/SGLang class names, `llamacppArchs` for GGUF arch tokens, `mlxTypes` for MLX
   `model_type`s). It is seeded generously, so a common architecture is usually already
   present; if `atlas inspect <repo>` reports `loadable: no`, add the architecture there
   too (the not-loadable message points at the same file).

## Earn it with a conformance case

A family entry ships **with a conformance case** — "earned by the suite, not vibes"
([ADR-0015](decisions/0015-bring-any-model-auto-configuration.md) Decision 2). The M8
Phase 5 gate auto-configures a catalog-less known-family model and runs the agent
tool-use / streaming groups against it; your PR adds (or extends) a case there so the
new family's parser config is proven to drive tool calls and reasoning correctly, not
just compiled in. A family without a passing conformance case is not considered
supported.

## Checklist

- [ ] `normalizeFamily` maps the model's `model_type`/architecture to the family token.
- [ ] A `families` entry with the correct per-engine parser names (verified against the
      engine's docs), `Reasoning` set correctly, and only verified engines listed.
- [ ] The architecture is in `arch.go` (`atlas inspect <repo>` shows `loadable: yes`).
- [ ] A conformance case proves the family tool-calls (and reasons, if applicable).
- [ ] `bash scripts/check.sh` is green.

See also: [m8-build-plan.md](m8-build-plan.md) (how auto-configuration fits the milestone)
and [conformance-suite.md](conformance-suite.md) (the suite your case joins).
