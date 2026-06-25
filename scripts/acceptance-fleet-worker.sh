#!/usr/bin/env bash
# acceptance-fleet-worker.sh — host B side of the multi-host M1 fleet acceptance.
#
# Runs ON the GPU box (host B). It is the counterpart to scripts/acceptance-fleet.sh
# (host A): host A starts the control plane and publishes a join bundle to SSM; this
# script reads that bundle and joins as a cross-host vLLM worker over wss://, then
# stays up until host A signals the run is done.
#
#   1. read {ip, port, pin, token, model} from SSM ($SSM_PREFIX/server)
#   2. start `atlas worker` on vLLM, dialing host A's wss:// endpoint with the pin
#   3. poll SSM for the done flag ($SSM_PREFIX/done), then exit (stopping the worker)
#
# The caller (the nightly workflow) is responsible for AWS credentials (the runner
# assumes the OIDC role before invoking this) and for tearing the box down after.
# See docs/m1-acceptance.md and the header of scripts/acceptance-fleet.sh for the
# baked-in decisions (same VPC/SG with the wss port open; SSM SecureString handoff).
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

SSM_PREFIX=${SSM_PREFIX:-/atlas/nightly/${GITHUB_RUN_ID:-local}}
AWS_REGION=${AWS_REGION:-eu-west-2}
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-acceptance-fleet-worker}

# vLLM needs the same GPU-fit + toolchain env as the single-node GPU track
# (scripts/acceptance.sh documents why each one is here).
export ATLAS_ENGINE_READY_TIMEOUT=${ATLAS_ENGINE_READY_TIMEOUT:-15m}
export VLLM_USE_FLASHINFER_SAMPLER=${VLLM_USE_FLASHINFER_SAMPLER:-0}
VLLM_ENGINE_ARGS=${VLLM_ENGINE_ARGS:-"--max-model-len 32768 --enforce-eager"}

BUNDLE_TIMEOUT=${BUNDLE_TIMEOUT:-600} # seconds to wait for host A to publish
DONE_TIMEOUT=${DONE_TIMEOUT:-3600}    # max seconds to stay joined before giving up

need() { command -v "$1" >/dev/null 2>&1 || { echo "fleet-worker: missing prerequisite '$1'" >&2; exit 2; }; }
need go
need uv
need aws
need jq

echo "==> Building atlas"
go build -o "$REPO/atlas" ./cmd/atlas

# --- wait for host A's join bundle -------------------------------------------
echo "==> Waiting up to ${BUNDLE_TIMEOUT}s for the join bundle ($SSM_PREFIX/server)"
BUNDLE="" ; waited=0
until BUNDLE=$(aws ssm get-parameter --region "$AWS_REGION" --name "$SSM_PREFIX/server" \
    --with-decryption --query 'Parameter.Value' --output text 2>/dev/null) && [[ -n "$BUNDLE" ]]; do
  if (( waited >= BUNDLE_TIMEOUT )); then
    echo "::error::host A never published the join bundle to $SSM_PREFIX/server" >&2; exit 1
  fi
  sleep 5; waited=$((waited + 5))
done

IP=$(echo "$BUNDLE" | jq -r .ip)
PORT=$(echo "$BUNDLE" | jq -r .port)
PIN=$(echo "$BUNDLE" | jq -r .pin)
TOKEN=$(echo "$BUNDLE" | jq -r .token)
MODEL=$(echo "$BUNDLE" | jq -r .model)
WSS="wss://${IP}:${PORT}/workers/connect"
echo "==> Joining $WSS as a vLLM worker serving $MODEL"

# --- start the vLLM worker ---------------------------------------------------
nvidia-smi || { echo "::error::no GPU visible on host B" >&2; exit 1; }

# Split engine args on whitespace into repeated --engine-arg flags.
declare -a eargs=()
for tok in $VLLM_ENGINE_ARGS; do eargs+=(--engine-arg "$tok"); done

ATLAS_TLS_PIN="$PIN" \
  "$REPO/atlas" worker \
    --join "$WSS" --token "$TOKEN" \
    --engine vllm --state-dir "$ATLAS_STATE_DIR" \
    --name hostB-vllm --model "$MODEL" "${eargs[@]}" >"$REPO/atlas-fleet-worker-vllm.log" 2>&1 &
WORKER_PID=$!
trap 'kill "$WORKER_PID" 2>/dev/null || true' EXIT

# --- stay up until host A signals done ---------------------------------------
echo "==> Worker started (pid $WORKER_PID); waiting for the done signal"
waited=0
while true; do
  if ! kill -0 "$WORKER_PID" 2>/dev/null; then
    echo "::error::vLLM worker exited before the run finished:" >&2
    tail -n 40 "$REPO/atlas-fleet-worker-vllm.log" >&2; exit 1
  fi
  DONE=$(aws ssm get-parameter --region "$AWS_REGION" --name "$SSM_PREFIX/done" \
    --query 'Parameter.Value' --output text 2>/dev/null || true)
  [[ "$DONE" == "1" ]] && { echo "==> Host A signalled done; stopping worker"; break; }
  if (( waited >= DONE_TIMEOUT )); then
    echo "::error::no done signal after ${DONE_TIMEOUT}s; stopping" >&2; exit 1
  fi
  sleep 10; waited=$((waited + 10))
done
echo "==> Fleet worker finished cleanly"
