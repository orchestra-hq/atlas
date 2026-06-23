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
# "done" (docs/open-questions.md, docs/roadmap.md M0.5).
#
# The model refs and vLLM parser flags below are defaults tuned for a single
# ~24GB GPU; they are the two items docs/open-questions.md flags as genuinely
# open until this run first executes. Override via env to tune without editing.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

# --- config (env, with defaults) --------------------------------------------
ENGINES=${ACCEPTANCE_ENGINES:-"vllm llamacpp"}
ADDR=${ATLAS_ADDR:-127.0.0.1:8080}
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-acceptance}
export CONF_CLAUDE_CODE_SMOKE=${CONF_CLAUDE_CODE_SMOKE:-1}
# The Claude Code smoke drives a full agentic loop; a capable model on a GPU
# still needs minutes, so give it more wall-clock than the per-PR default (300s).
export CONF_CLAUDE_CODE_TIMEOUT=${CONF_CLAUDE_CODE_TIMEOUT:-600}
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
# fit only. On a 24 GB card a 15.3 GB Qwen3-8B leaves little headroom, so cap the
# served length (8192 is ample for conformance's short prompts) and use
# --enforce-eager: CUDA-graph capture is VRAM-hungry and was where vLLM was
# OOM-killed right after model load.
VLLM_ENGINE_ARGS=${VLLM_ENGINE_ARGS:-"--max-model-len 8192 --enforce-eager"}

# llama.cpp deploys TWO models (both Q4 fit a 24 GB card easily): a capable
# non-reasoning chat model for the sonnet/haiku aliases — which also drives the
# Claude Code smoke (G9) and satisfies G4's non-reasoning-graceful assertion that
# a hybrid model cannot — and the reasoning Qwen3-8B for opus (G4 reasoning half).
# vLLM stays single-model: two 7-8B bf16 models exceed 24 GB, so its non-reasoning
# coverage waits on quantized weights (tracked separately).
LLAMACPP_MODEL=${LLAMACPP_MODEL:-qwen2.5-7b-instruct-gguf}
LLAMACPP_REASONING_MODEL=${LLAMACPP_REASONING_MODEL:-qwen3-8b-gguf}
LLAMACPP_ENGINE_ARGS=${LLAMACPP_ENGINE_ARGS:-""}

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

  local rc=0
  ( cd conformance && uv run --locked python run.py \
      --base-url "http://${ADDR}" \
      --engine "$engine" \
      --model "$SONNET_ALIAS" \
      --reasoning-model "$OPUS_ALIAS" \
      --require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10 \
      --output "results/matrix-${engine}.json" ) || rc=$?

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
