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
READY_TIMEOUT=${READY_TIMEOUT:-600} # seconds; an 8B vLLM load is minutes-slow

# Capable models per engine. vLLM takes an HF repo id; llama.cpp takes an -hf
# spec (repo[:quant]) it downloads, or a local .gguf path. The reasoning model
# may equal the chat model (Qwen3 is hybrid-thinking) — then it is launched once
# and both the sonnet and opus aliases resolve to it.
VLLM_MODEL=${VLLM_MODEL:-Qwen/Qwen3-8B}
VLLM_REASONING_MODEL=${VLLM_REASONING_MODEL:-$VLLM_MODEL}
# vLLM needs the model's tool/reasoning parser flags (catalog records these for
# catalog models; this model is passed directly, so set them here). VALIDATE on
# first run against the pinned vLLM (internal/runtime.VLLMVersion).
VLLM_ENGINE_ARGS=${VLLM_ENGINE_ARGS:-"--tool-call-parser hermes --reasoning-parser qwen3"}

LLAMACPP_MODEL=${LLAMACPP_MODEL:-Qwen/Qwen3-8B-GGUF:Q4_K_M}
LLAMACPP_REASONING_MODEL=${LLAMACPP_REASONING_MODEL:-$LLAMACPP_MODEL}
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

if (( overall == 0 )); then
  echo
  echo "==> Acceptance GREEN on: $ENGINES"
else
  echo
  echo "==> Acceptance had failures (see above)" >&2
fi
exit $overall
