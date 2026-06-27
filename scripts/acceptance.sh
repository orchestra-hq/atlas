#!/usr/bin/env bash
# acceptance.sh — the provider-agnostic Atlas acceptance run (M0.5, ADR-0006).
#
# This is stage 2 of the three decoupled acceptance stages:
#
#   1. provision a GPU host   (SkyPilot / machulav EC2 / a box you launched)
#   2. run THIS script        (knows nothing about how the host was created)
#   3. tear the host down     (the caller again)
#
# Run it ON a host that already has the prerequisites installed — Go, uv, Node,
# and (for the Claude Code smoke) the `claude` binary — plus a CUDA GPU for the
# vLLM engine. SkyPilot, an ephemeral EC2 runner, and a hand-launched box all
# drive it identically; swapping the provisioner changes nothing here. Stages 1
# and 3 live in examples/acceptance/atlas-acceptance.sky.yaml and the nightly
# workflow (.github/workflows/nightly-gpu.yml).
#
# For each requested engine it boots `atlas up` on a capable model, then runs
# the FULL conformance gate (G1–G10) plus the real Claude Code drop-in smoke
# (CONF_CLAUDE_CODE_SMOKE=1). A green run on BOTH engines is what flips M0 to
# "done" (docs/internal/open-questions.md, docs/roadmap.md M0.5).
#
# The model refs and vLLM parser flags below are defaults tuned for a single
# ~24GB GPU; they are the two items docs/internal/open-questions.md flags as genuinely
# open until this run first executes. Override via env to tune without editing.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

# --- config (env, with defaults) --------------------------------------------
ENGINES=${ACCEPTANCE_ENGINES:-"vllm llamacpp"}
ADDR=${ATLAS_ADDR:-127.0.0.1:8080}
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-acceptance}
export CONF_CLAUDE_CODE_SMOKE=${CONF_CLAUDE_CODE_SMOKE:-1}
# Conformance groups the gate requires. The single-node GPU/CPU tracks prove the
# full M0 surface incl. the G9 agent harness; the MLX/SGLang G17 cells scope to
# G1–G8,G10 (no G9 — their 7–8B models drive an agent loop only intermittently)
# and run with CONF_CLAUDE_CODE_SMOKE=0. See docs/internal/m2-acceptance.md.
CONF_REQUIRE=${CONF_REQUIRE:-G1,G2,G3,G4,G5,G6,G7,G8,G9,G10}
# The Claude Code smoke drives a full agentic loop; a capable model on a GPU
# still needs minutes, so give it more wall-clock than the per-PR default (300s).
export CONF_CLAUDE_CODE_TIMEOUT=${CONF_CLAUDE_CODE_TIMEOUT:-600}
# Per-test bound for the TS (vitest) conformance suite, in seconds. A 12B reasoning
# model on the CPU track streams one thinking response in ~30s+ (it was tripping
# vitest's 30s default — a timeout there looks identical to "no thinking block"),
# so give the slow tracks ample headroom. Consumed by conformance/ts/vitest.config.ts.
export CONF_TS_TIMEOUT=${CONF_TS_TIMEOUT:-300}
READY_TIMEOUT=${READY_TIMEOUT:-900} # seconds the script polls /readyz for
# atlas's own per-engine startup timeout (worker default is 3m). vLLM cold start
# downloads + loads a multi-GB model, which exceeds 3m, so raise it; the script's
# READY_TIMEOUT poll above must stay >= this.
export ATLAS_ENGINE_READY_TIMEOUT=${ATLAS_ENGINE_READY_TIMEOUT:-15m}
# vLLM's FlashInfer sampler JIT-compiles a CUDA kernel at startup via `ninja`,
# which the runner has no build toolchain for ("FileNotFoundError: 'ninja'").
# Force the native PyTorch sampler instead — no JIT, identical sampling. Harmless
# for the other engines (vLLM-only env).
export VLLM_USE_FLASHINFER_SAMPLER=${VLLM_USE_FLASHINFER_SAMPLER:-0}

# Capable models per engine, served by CATALOG NAME (not a raw -hf/spec). The
# catalog path threads the model's reasoning flag (so enable_thinking is gated
# per request — a raw-served hybrid otherwise leaks <think> blocks into
# non-reasoning replies) and its tool/reasoning parser engine_args (vLLM needs
# --enable-auto-tool-choice, which a raw spec omitted → engine start failed). The
# chat and reasoning models are one hybrid model (Qwen3), launched once with both
# the sonnet and opus aliases resolving to it. See catalog/starter.yaml.
VLLM_MODEL=${VLLM_MODEL:-qwen3-8b}
VLLM_REASONING_MODEL=${VLLM_REASONING_MODEL:-$VLLM_MODEL}
# Parser flags now come from the catalog; this env is for acceptance-GPU memory
# fit only. --enforce-eager skips CUDA-graph capture (VRAM-hungry; was OOM-killing
# vLLM right after model load on the 24 GB card). --max-model-len 32768 fits the
# real Claude Code smoke — its system prompt + tool definitions are ~24k tokens,
# so the earlier 8192 gave "Prompt is too long" before the loop could start; the
# KV cache holds ~34k tokens at this util, so 32768 fits one sequence.
VLLM_ENGINE_ARGS=${VLLM_ENGINE_ARGS:-"--max-model-len 32768 --enforce-eager"}

# llama.cpp deploys TWO models on the CPU track (both Q4, fit the c7i's 32 GB):
# a capable non-reasoning chat model (qwen2.5-7b) for sonnet/haiku — satisfying
# G4's non-reasoning-graceful assertion a hybrid model cannot — and the reasoning
# **gemma-4-12b-coder** for opus, which gets full G1–G10 coverage incl. its
# thinking path (validated locally: thinking blocks + tool calls both work on
# llama.cpp, unlike Qwen3's truncated-reasoning gap on vLLM's parser). gemma is
# GGUF-only, so llama.cpp/CPU is its home (no clean GPU path) — hence it lives on
# this track. Override to qwen3-8b-gguf if you want the Qwen reasoning model back.
LLAMACPP_MODEL=${LLAMACPP_MODEL:-qwen2.5-7b-instruct-gguf}
LLAMACPP_REASONING_MODEL=${LLAMACPP_REASONING_MODEL:-gemma-4-12b-coder}
LLAMACPP_ENGINE_ARGS=${LLAMACPP_ENGINE_ARGS:-""}

# SGLang (NVIDIA GPU nightly cell, M2 G17). A single 24 GB L4 can't hold two
# unquantized 7–8B models (≈30 GB combined → CUDA OOM loading the second), so —
# exactly like the vLLM track — SGLang serves ONE hybrid reasoning model
# (qwen3-8b) for every tier alias. G4's thinking path is covered; its
# non-reasoning graceful case skips (no separate non-reasoning model), as on vLLM.
# The catalog entry carries SGLang's --tool-call-parser (qwen25) + --reasoning-parser
# (qwen3). SGLANG_ENGINE_ARGS covers GPU fit and the no-build-toolchain runner:
# --mem-fraction-static caps the KV-cache pool, --context-length sets the window.
# SGLang JIT-compiles core kernels (fused RoPE, attention) at startup via `ninja`
# regardless of backend — the nightly job installs ninja for that — and the
# triton attention + pytorch sampling backends avoid the *extra* flashinfer JIT
# (Triton compiles through its own runtime).
SGLANG_MODEL=${SGLANG_MODEL:-qwen3-8b-sglang}
SGLANG_REASONING_MODEL=${SGLANG_REASONING_MODEL:-qwen3-8b-sglang}
SGLANG_ENGINE_ARGS=${SGLANG_ENGINE_ARGS:-"--mem-fraction-static 0.85 --context-length 32768 --attention-backend triton --sampling-backend pytorch"}

# MLX (Apple-Silicon nightly cell, M2 G17). The shipped MLX catalog tier is
# non-reasoning only, so there is no distinct reasoning model: the 7B serves
# every alias and G4's thinking cases skip (reasoning_model fixture), while its
# graceful-degradation case and every other group run. mlx_lm.server takes no
# extra args here.
# MLX_REASONING_MODEL is intentionally empty: no reasoning tier in the catalog,
# so run_engine omits --reasoning-model (G4 thinking skips) and points G4's
# graceful case at the lone non-reasoning 7B.
MLX_MODEL=${MLX_MODEL:-qwen2.5-7b-instruct-mlx}
MLX_REASONING_MODEL=${MLX_REASONING_MODEL:-}
MLX_ENGINE_ARGS=${MLX_ENGINE_ARGS:-""}

# Aliases are constant across engines, so the harness always addresses the same
# logical names regardless of which engine is serving them.
SONNET_ALIAS=claude-sonnet-4-6
HAIKU_ALIAS=claude-haiku-4-5
OPUS_ALIAS=claude-opus-4-1

# --- prerequisites -----------------------------------------------------------
need() { command -v "$1" >/dev/null 2>&1 || { echo "acceptance: missing prerequisite '$1'" >&2; exit 2; }; }
need go
need uv
need npm
if [[ "$CONF_CLAUDE_CODE_SMOKE" == "1" ]]; then need claude; fi

echo "==> Building atlas"
go build -o "$REPO/atlas" ./cmd/atlas

# API keys replaced the shared --api-key secret in phase 5 (ADR-0008): mint one
# into the state-dir store the gateway reads, and hand it to the harness via the
# env (run.py reads ATLAS_API_KEY).
echo "==> Provisioning API key"
export ATLAS_API_KEY=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" --quiet)

echo "==> Installing conformance TS deps"
( cd conformance/ts && npm ci --no-fund --no-audit --loglevel=error )

# --- per-engine acceptance ---------------------------------------------------
# Launch atlas up for one engine, wait for readiness, run the full harness, then
# always stop the engine. Returns the harness exit code.
run_engine() {
  local engine=$1
  local upper model rmodel eargs
  upper=$(printf '%s' "$engine" | tr '[:lower:]' '[:upper:]')
  local mv="${upper}_MODEL" rv="${upper}_REASONING_MODEL" av="${upper}_ENGINE_ARGS"
  model=${!mv}
  rmodel=${!rv}
  eargs=${!av}

  echo
  echo "===================================================================="
  echo "  Acceptance: engine=$engine model=$model reasoning=$rmodel"
  echo "===================================================================="

  # An empty <ENGINE>_REASONING_MODEL means the engine has no reasoning model
  # (the MLX catalog tier is non-reasoning only). The opus alias then resolves to
  # the chat model, --reasoning-model is omitted (so G4's thinking cases skip via
  # the reasoning_model fixture), and the lone non-reasoning model drives G4's
  # graceful-degradation case instead.
  local has_reasoning=1
  if [[ -z "$rmodel" ]]; then has_reasoning=0; rmodel=$model; fi

  # Unique --model list (the reasoning model is the same model when hybrid).
  local -a up_args=(up --engine "$engine" --addr "$ADDR")
  up_args+=(--model "$model")
  if [[ "$rmodel" != "$model" ]]; then up_args+=(--model "$rmodel"); fi
  up_args+=(--alias "${SONNET_ALIAS}=${model}")
  up_args+=(--alias "${HAIKU_ALIAS}=${model}")
  up_args+=(--alias "${OPUS_ALIAS}=${rmodel}")
  # Split engine args on whitespace, one --engine-arg per token.
  local tok
  for tok in $eargs; do up_args+=(--engine-arg "$tok"); done

  local log="$REPO/atlas-${engine}.log"
  "$REPO/atlas" "${up_args[@]}" >"$log" 2>&1 &
  local pid=$!
  # shellcheck disable=SC2064
  trap "kill $pid 2>/dev/null || true" RETURN

  echo "==> Waiting up to ${READY_TIMEOUT}s for readiness (pid $pid)"
  local waited=0
  until curl -sf -o /dev/null "http://${ADDR}/readyz"; do
    if ! kill -0 "$pid" 2>/dev/null; then echo "atlas exited early:" >&2; cat "$log" >&2; return 1; fi
    if (( waited >= READY_TIMEOUT )); then echo "atlas not ready after ${READY_TIMEOUT}s:" >&2; cat "$log" >&2; return 1; fi
    sleep 3; waited=$((waited + 3))
  done
  echo "==> Ready after ${waited}s"

  # G4's graceful-degradation case needs a genuinely non-reasoning model. When
  # the chat model differs from the reasoning model (the CPU track serves a
  # distinct non-reasoning chat model, e.g. qwen2.5-7b, for sonnet/haiku), the
  # sonnet alias IS that model — point the case at it. When one hybrid serves
  # every tier (the vLLM track: Qwen3 for all aliases), there is no non-reasoning
  # model, so the arg is omitted and the case skips rather than false-failing.
  # G4 gets a non-reasoning model when one is deployed: either a distinct chat
  # model (model != rmodel) or the lone non-reasoning model of an engine with no
  # reasoning tier (has_reasoning==0). A single hybrid (model==rmodel, reasoning)
  # omits it and the case skips. --reasoning-model is omitted entirely when the
  # engine has no reasoning model, so G4's thinking cases skip rather than fail.
  # Build one never-empty argv. The optional --reasoning-model/--nonreasoning-model
  # are appended in place rather than splatting separate arrays: macOS ships bash
  # 3.2 (the MLX runner), where expanding an *empty* array under `set -u` errors
  # ("unbound variable") — appending to a single non-empty array sidesteps that.
  local -a run_args=(--base-url "http://${ADDR}" --engine "$engine" --model "$SONNET_ALIAS")
  if [[ "$has_reasoning" -eq 1 ]]; then run_args+=(--reasoning-model "$OPUS_ALIAS"); fi
  if [[ "$model" != "$rmodel" || "$has_reasoning" -eq 0 ]]; then
    run_args+=(--nonreasoning-model "$SONNET_ALIAS")
  fi
  run_args+=(--require "$CONF_REQUIRE" --output "results/matrix-${engine}.json")

  local rc=0
  ( cd conformance && uv run --locked python run.py "${run_args[@]}" ) || rc=$?

  kill "$pid" 2>/dev/null || true
  trap - RETURN
  return $rc
}

overall=0
for engine in $ENGINES; do
  if ! run_engine "$engine"; then
    echo "!! Acceptance FAILED for engine=$engine" >&2
    overall=1
  else
    echo "++ Acceptance passed for engine=$engine"
  fi
done

# Aggregate the per-engine runs into the published agent-capability matrix (G18,
# M2 phase 4c). Each engine wrote results/matrix-<engine>.json above; the
# generator turns them into one capability-matrix.json + a Markdown table. This
# is best-effort reporting — a missing input (an engine that never produced a
# matrix) does not change the acceptance verdict, which $overall already holds.
matrices=()
for engine in $ENGINES; do
  # Paths are relative to conformance/, where the generator runs.
  [[ -f "$REPO/conformance/results/matrix-${engine}.json" ]] && matrices+=("results/matrix-${engine}.json")
done
if (( ${#matrices[@]} > 0 )); then
  echo
  echo "==> Generating agent-capability matrix (G18)"
  ( cd conformance && uv run --locked python capability_matrix.py \
      "${matrices[@]}" \
      --output results/capability-matrix.json \
      --markdown results/CAPABILITY.md ) || true
fi

if (( overall == 0 )); then
  echo
  echo "==> Acceptance GREEN on: $ENGINES"
else
  echo
  echo "==> Acceptance had failures (see above)" >&2
fi
exit $overall
