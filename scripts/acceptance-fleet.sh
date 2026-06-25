#!/usr/bin/env bash
# acceptance-fleet.sh — the multi-host M1 fleet acceptance run (stage 2).
#
# Runs ON host A (the control-plane host). It is the fleet analogue of
# scripts/acceptance.sh: the same three-stage decoupling (provision → run this →
# teardown) applies, but the topology spans two machines, so this script also
# coordinates a remote worker over AWS SSM Parameter Store.
#
# Topology (see docs/m1-acceptance.md — the decided heterogeneous two-engine fleet):
#
#   host A (here): atlas server --tls-self-signed   +   a co-located llama.cpp worker
#   host B (GPU) : atlas worker on vLLM, dials host A's wss:// endpoint cross-host
#
# What this script does on host A:
#   1. start `atlas server --tls-self-signed` (control plane only, TLS on)
#   2. mint an admin API key, capture the cert pin from the banner
#   3. publish {private-ip, port, pin, join-token, api-key} to SSM so host B can join
#   4. start the co-located llama.cpp worker (joins over wss:// with the pin)
#   5. wait for BOTH models to register — the local llama model and host B's vLLM model
#   6. gate G1–G10 over the fleet (harness aliases point at the cross-host vLLM worker)
#   7. replay the G11–G14 fleet scenarios (auth, usage, drain, kill-9, routing)
#   8. signal "done" on SSM (so host B stops its worker) and tear down
#
# DECISIONS BAKED IN HERE (the project owner's calls, 2026-06-25):
#   • Cross-host reachability: host A and host B sit in the SAME VPC / security
#     group, with the server's wss port (default 9443) open between members. Host B
#     dials host A's PRIVATE IP (this script advertises `hostname -I`'s first addr).
#   • Secret handoff: the pin / join-token / API-key travel host A → host B via SSM
#     Parameter Store (a SecureString under $SSM_PREFIX), not job outputs — the two
#     acceptance jobs run concurrently on separate runners, so they rendezvous on SSM.
#     IAM the runner role needs (account-side, see examples/acceptance/README.md):
#     ssm:PutParameter / GetParameter / DeleteParameter on
#     arn:aws:ssm:*:*:parameter/atlas/nightly/* and kms:Encrypt / kms:Decrypt on the
#     aws/ssm managed key (for the SecureString). The params are short-lived and
#     deleted at the end of the run.
#
# Topology-flexible for local verification: FLEET_REMOTE_WORKER=0 launches a SECOND
# LOCAL llama.cpp worker in place of host B (no SSM, no GPU) so the script's gate +
# scenarios can be smoke-tested on one box. CI sets FLEET_REMOTE_WORKER=1.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

# --- config (env, with defaults) --------------------------------------------
PORT=${ATLAS_FLEET_PORT:-9443}
ADDR=${ATLAS_ADDR:-0.0.0.0:$PORT} # must be reachable by host B (bind all ifaces)
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-acceptance-fleet}
FLEET_REMOTE_WORKER=${FLEET_REMOTE_WORKER:-1}

# Coordination namespace + region for the SSM rendezvous (remote mode only).
SSM_PREFIX=${SSM_PREFIX:-/atlas/nightly/${GITHUB_RUN_ID:-local}}
AWS_REGION=${AWS_REGION:-eu-west-2}

# The join token host B must present. Generated if unset; published via SSM.
FLEET_TOKEN=${ATLAS_JOIN_TOKEN:-fleet-$(date +%s)-$RANDOM}

# Host A's local engine: a llama.cpp gguf model (the co-located worker).
LLAMACPP_MODEL=${LLAMACPP_MODEL:-qwen2.5-7b-instruct-gguf}
# Host B's engine: the vLLM model the cross-host GPU worker serves. Host A only
# needs the NAME (to alias to it and to wait for its route); host B serves it.
VLLM_MODEL=${VLLM_MODEL:-qwen3-8b}

# Tier aliases point at the cross-host vLLM model, so the G1–G10 harness exercises
# the remote GPU worker over the wss channel — the new dimension the per-PR loopback
# job does not cover. The llama.cpp model is addressed by its own name for the G11
# multi-worker-routing scenario.
SONNET_ALIAS=claude-sonnet-4-6
HAIKU_ALIAS=claude-haiku-4-5
OPUS_ALIAS=claude-opus-4-1

# vLLM cold download+load far exceeds the worker's 3m default; the remote worker
# sets its own ATLAS_ENGINE_READY_TIMEOUT, but host A must wait at least as long
# for the model's route to appear.
READY_TIMEOUT=${READY_TIMEOUT:-1200} # seconds host A polls for both models
export CONF_TS_TIMEOUT=${CONF_TS_TIMEOUT:-300}
HEARTBEAT_WINDOW=${HEARTBEAT_WINDOW:-60} # max seconds a kill-9 may take to unblock

# --- prerequisites -----------------------------------------------------------
need() { command -v "$1" >/dev/null 2>&1 || { echo "acceptance-fleet: missing prerequisite '$1'" >&2; exit 2; }; }
need go
need uv
need npm
need curl
need jq
if [[ "$FLEET_REMOTE_WORKER" == "1" ]]; then need aws; fi

# Endpoints. The harness + scenarios reach the gateway over https on loopback; the
# worker(s) dial the advertised private IP so the wss + pinned-cert path is real.
PRIVATE_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[[ -n "$PRIVATE_IP" ]] || PRIVATE_IP=127.0.0.1
API="https://127.0.0.1:$PORT"
WSS="wss://${PRIVATE_IP}:${PORT}/workers/connect"
CERT="$ATLAS_STATE_DIR/tls/cert.pem"

# SDKs verify TLS against the self-signed cert via these standard env vars (httpx
# / requests honour *_CA_*; Node honours NODE_EXTRA_CA_CERTS). Set once the cert
# exists; run.py inherits them for pytest + vitest.
set_ca_env() {
  export SSL_CERT_FILE="$CERT" REQUESTS_CA_BUNDLE="$CERT" NODE_EXTRA_CA_CERTS="$CERT"
}

# --- build + key -------------------------------------------------------------
echo "==> Building atlas"
go build -o "$REPO/atlas" ./cmd/atlas

echo "==> Provisioning admin API key"
export ATLAS_API_KEY=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" --admin --quiet)

echo "==> Installing conformance TS deps"
( cd conformance/ts && npm ci --no-fund --no-audit --loglevel=error )

# --- teardown ----------------------------------------------------------------
SERVER_PID="" ; declare -a WORKER_PIDS=()
cleanup() {
  # Signal host B to stop, then best-effort SSM cleanup.
  if [[ "$FLEET_REMOTE_WORKER" == "1" ]]; then
    aws ssm put-parameter --region "$AWS_REGION" --name "$SSM_PREFIX/done" \
      --type String --overwrite --value "1" >/dev/null 2>&1 || true
    aws ssm delete-parameter --region "$AWS_REGION" --name "$SSM_PREFIX/server" >/dev/null 2>&1 || true
  fi
  local p
  for p in "${WORKER_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT

# --- start the control plane -------------------------------------------------
echo
echo "===================================================================="
echo "  Fleet acceptance: server on host A ($PRIVATE_IP:$PORT), TLS self-signed"
echo "  remote vLLM worker: $([[ $FLEET_REMOTE_WORKER == 1 ]] && echo "host B (via SSM $SSM_PREFIX)" || echo "LOCAL (smoke mode)")"
echo "===================================================================="

SERVER_LOG="$REPO/atlas-fleet-server.log"
export ATLAS_LOG_FILE="$SERVER_LOG" # G10 token counts live in the server log
"$REPO/atlas" server \
  --addr "$ADDR" --state-dir "$ATLAS_STATE_DIR" \
  --token "$FLEET_TOKEN" --tls-self-signed \
  --autostart-timeout 0 --idle-timeout 0 \
  --alias "${SONNET_ALIAS}=${VLLM_MODEL}" \
  --alias "${HAIKU_ALIAS}=${VLLM_MODEL}" \
  --alias "${OPUS_ALIAS}=${VLLM_MODEL}" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# Capture the cert pin the server prints in its banner (sha256:<64-hex>).
echo "==> Waiting for the server's TLS pin"
PIN=""
for _ in $(seq 1 30); do
  PIN=$(grep -oE 'sha256:[0-9a-f]{64}' "$SERVER_LOG" | head -1 || true)
  [[ -n "$PIN" ]] && break
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "server exited early:" >&2; cat "$SERVER_LOG" >&2; exit 1; }
  sleep 1
done
[[ -n "$PIN" ]] || { echo "no TLS pin in server banner:" >&2; cat "$SERVER_LOG" >&2; exit 1; }
echo "    pin=$PIN"
[[ -f "$CERT" ]] || { echo "self-signed cert not found at $CERT" >&2; exit 1; }
set_ca_env

# --- publish the join bundle for host B --------------------------------------
if [[ "$FLEET_REMOTE_WORKER" == "1" ]]; then
  echo "==> Publishing join bundle to SSM ($SSM_PREFIX/server)"
  BUNDLE=$(jq -n --arg ip "$PRIVATE_IP" --arg port "$PORT" --arg pin "$PIN" \
    --arg token "$FLEET_TOKEN" --arg key "$ATLAS_API_KEY" --arg model "$VLLM_MODEL" \
    '{ip:$ip, port:$port, pin:$pin, token:$token, api_key:$key, model:$model}')
  aws ssm put-parameter --region "$AWS_REGION" --name "$SSM_PREFIX/server" \
    --type SecureString --overwrite --value "$BUNDLE" >/dev/null
fi

# --- start the co-located llama.cpp worker -----------------------------------
# Sets LAST_WORKER_PID (and tracks it for cleanup). Not called via $() — that runs
# in a subshell, which would lose the WORKER_PIDS append.
LAST_WORKER_PID=""
start_llama_worker() { # $1 = name, $2 = log
  "$REPO/atlas" worker \
    --join "$WSS" --token "$FLEET_TOKEN" --tls-pin "$PIN" \
    --engine llamacpp --state-dir "$ATLAS_STATE_DIR" \
    --name "$1" --model "$LLAMACPP_MODEL" >"$2" 2>&1 &
  LAST_WORKER_PID=$!
  WORKER_PIDS+=("$LAST_WORKER_PID")
}

echo "==> Pulling the local llama.cpp model"
"$REPO/atlas" pull "$LLAMACPP_MODEL" --state-dir "$ATLAS_STATE_DIR" >/dev/null

echo "==> Starting co-located llama.cpp worker"
start_llama_worker hostA-llama "$REPO/atlas-fleet-worker-llama.log"
LLAMA_PID=$LAST_WORKER_PID

# In local smoke mode there is no host B — stand in a second local llama worker
# serving the "vLLM" model name so routing/gate scenarios have a second route.
if [[ "$FLEET_REMOTE_WORKER" != "1" ]]; then
  echo "==> [smoke] Starting a second local worker as the stand-in remote ($VLLM_MODEL)"
  "$REPO/atlas" pull "$VLLM_MODEL" --state-dir "$ATLAS_STATE_DIR" >/dev/null 2>&1 || \
    VLLM_MODEL="$LLAMACPP_MODEL" # fall back if the vLLM model isn't a local gguf
  "$REPO/atlas" worker \
    --join "$WSS" --token "$FLEET_TOKEN" --tls-pin "$PIN" \
    --engine llamacpp --state-dir "$ATLAS_STATE_DIR" \
    --name hostB-stand-in --model "$VLLM_MODEL" >"$REPO/atlas-fleet-worker-standin.log" 2>&1 &
  WORKER_PIDS+=($!)
fi

# --- wait for both models to be routable -------------------------------------
# A small authed request returns the model's HTTP status; 200 == routable.
ask() { # $1 = model name
  curl -sS -m 60 -o /dev/null -w '%{http_code}' --cacert "$CERT" \
    -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
    "$API/v1/messages" \
    -d "$(printf '{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' "$1")" || true
}

echo "==> Waiting up to ${READY_TIMEOUT}s for both models to register (local + remote)"
waited=0
until [[ "$(ask "$LLAMACPP_MODEL")" == "200" && "$(ask "$VLLM_MODEL")" == "200" ]]; do
  if (( waited >= READY_TIMEOUT )); then
    echo "::error::both fleet models did not become routable within ${READY_TIMEOUT}s" >&2
    echo "=== server ==="; cat "$SERVER_LOG" >&2
    echo "=== llama worker ==="; cat "$REPO/atlas-fleet-worker-llama.log" >&2
    exit 1
  fi
  sleep 5; waited=$((waited + 5))
done
echo "==> Both models routable after ${waited}s ($LLAMACPP_MODEL local, $VLLM_MODEL remote)"

# ============================================================================
#  Acceptance criteria (docs/m1-acceptance.md)
# ============================================================================
overall=0
fail() { echo "::error::$*" >&2; overall=1; }

# --- (1) Full surface over the cross-host worker: G1–G10 --------------------
echo
echo "==> [criterion 1] G1–G10 over the fleet (aliases → cross-host vLLM worker)"
( cd conformance && uv run --locked python run.py \
    --base-url "$API" \
    --engine vllm \
    --model "$SONNET_ALIAS" \
    --reasoning-model "$OPUS_ALIAS" \
    --require G1,G2,G3,G4,G5,G6,G7,G8,G9,G10 \
    --output "results/matrix-fleet.json" ) || fail "G1–G10 gate failed over the fleet"

# --- (3) G12 auth over the real https endpoint ------------------------------
echo
echo "==> [criterion 3] G12 auth"
code() { curl -sS -m 30 -o /dev/null -w '%{http_code}' --cacert "$CERT" "$@" || true; }
msg_body=$(printf '{"model":"%s","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' "$SONNET_ALIAS")
NOKEY=$(code -H 'content-type: application/json' "$API/v1/messages" -d "$msg_body")
BADKEY=$(code -H 'x-api-key: not-a-real-key' -H 'content-type: application/json' "$API/v1/messages" -d "$msg_body")
OKKEY=$(code -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' "$API/v1/messages" -d "$msg_body")
echo "    no-key=$NOKEY bad-key=$BADKEY valid-key=$OKKEY"
[[ "$NOKEY" == "401" ]] || fail "missing key should be 401 (got $NOKEY)"
[[ "$BADKEY" == "401" ]] || fail "invalid key should be 401 (got $BADKEY)"
[[ "$OKKEY" == "200" ]] || fail "valid key should be 200 (got $OKKEY)"
# Allowlist: a key scoped to only the llama model must be 403 on the vLLM-aliased one.
RESTRICTED=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" --quiet --allow "$LLAMACPP_MODEL" 2>/dev/null || true)
if [[ -n "$RESTRICTED" ]]; then
  DENIED=$(code -H "x-api-key: $RESTRICTED" -H 'content-type: application/json' "$API/v1/messages" -d "$msg_body")
  echo "    allowlisted-key on disallowed model=$DENIED"
  [[ "$DENIED" == "403" ]] || fail "allowlist should deny the disallowed model with 403 (got $DENIED)"
  # A non-admin client key must be refused on the admin surface.
  ADMINREJ=$(code -H "x-api-key: $RESTRICTED" "$API/admin/workers")
  [[ "$ADMINREJ" == "403" ]] || fail "non-admin key should be 403 on /admin/* (got $ADMINREJ)"
else
  echo "    (skipping allowlist case — --allow not available)"
fi

# --- (4) G13 usage metering across the fleet --------------------------------
echo
echo "==> [criterion 4] G13 usage metering (attributed to remote workers)"
"$REPO/atlas" usage --state-dir "$ATLAS_STATE_DIR" || true
USAGE=$("$REPO/atlas" usage --state-dir "$ATLAS_STATE_DIR" --json)
MODELS=$(echo "$USAGE" | jq '.by_model | length')
OUT=$(echo "$USAGE" | jq '[.by_model[].output_tokens] | add // 0')
REMOTE=$(echo "$USAGE" | jq '[.by_worker[].group | select(. != "local")] | length')
echo "    models=$MODELS output_tokens=$OUT remote_workers=$REMOTE"
{ [[ "$MODELS" -ge 1 && "$OUT" -ge 1 ]]; } || fail "usage ledger empty (models=$MODELS out=$OUT)"
[[ "$REMOTE" -ge 1 ]] || fail "no usage attributed to a remote worker ($(echo "$USAGE" | jq -c .by_worker))"

# --- (5) G14 fleet ops: drain + heartbeat timeout ---------------------------
# These operate on the co-located llama.cpp worker (host A controls its PID); the
# cross-host wss+TLS join itself is already proven by the remote worker serving 200.
echo
echo "==> [criterion 5] G14 drain + heartbeat-timeout (on the local worker)"
payload() { printf '{"model":"%s","max_tokens":%s,"messages":[{"role":"user","content":"%s"}]}' "$LLAMACPP_MODEL" "$1" "$2"; }

echo "    -- drain: in-flight completes, new requests refused --"
( curl -sS -m 120 -o /tmp/drain_body.txt -w '%{http_code}' --cacert "$CERT" \
    -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
    "$API/v1/messages" -d "$(payload 512 'Write a long detailed paragraph about the history of computing.')" \
    > /tmp/drain_code.txt ) &
DRAIN_REQ=$!
sleep 3
kill -TERM "$LLAMA_PID"
sleep 2
NEW=$(curl -sS -m 30 -o /dev/null -w '%{http_code}' --cacert "$CERT" \
  -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
  "$API/v1/messages" -d "$(payload 16 'hi')" || true)
wait "$DRAIN_REQ" || true
DRAIN_CODE=$(cat /tmp/drain_code.txt)
echo "    in-flight=$DRAIN_CODE post-drain-new=$NEW"
[[ "$DRAIN_CODE" == "200" ]] || fail "drain did not let the in-flight request finish (got $DRAIN_CODE)"
[[ "$NEW" != "200" ]] || fail "a new request was accepted after drain (got $NEW)"

echo "    -- heartbeat timeout: kill -9 unblocks an in-flight request with a retryable 5xx --"
# Bring a fresh llama worker back (the drained one exited). Test env-var join here
# too (criterion 5: ATLAS_SERVER_URL/ATLAS_JOIN_TOKEN/ATLAS_TLS_PIN, no flags).
ATLAS_SERVER_URL="$WSS" ATLAS_JOIN_TOKEN="$FLEET_TOKEN" ATLAS_TLS_PIN="$PIN" \
  "$REPO/atlas" worker --engine llamacpp --state-dir "$ATLAS_STATE_DIR" \
  --name hostA-llama-2 --model "$LLAMACPP_MODEL" >"$REPO/atlas-fleet-worker-llama2.log" 2>&1 &
LLAMA_PID=$!
WORKER_PIDS+=("$LLAMA_PID")
for _ in $(seq 1 150); do [[ "$(ask "$LLAMACPP_MODEL")" == "200" ]] && break; sleep 2; done
[[ "$(ask "$LLAMACPP_MODEL")" == "200" ]] || fail "worker did not rejoin via env-var join for the timeout scenario"
( curl -sS -m 90 -o /tmp/to_body.txt -w '%{http_code}' --cacert "$CERT" \
    -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
    "$API/v1/messages" -d "$(payload 512 'Write a long detailed essay about distributed systems.')" \
    > /tmp/to_code.txt ) &
TO_REQ=$!
sleep 3
start=$(date +%s)
kill -9 "$LLAMA_PID"
wait "$TO_REQ" || true
elapsed=$(( $(date +%s) - start ))
TO_CODE=$(cat /tmp/to_code.txt)
echo "    in-flight after kill-9 → $TO_CODE in ${elapsed}s"
{ [[ -n "$TO_CODE" && "$TO_CODE" -ge 500 ]]; } || fail "expected a retryable 5xx after kill-9 (got '$TO_CODE')"
[[ "$elapsed" -le "$HEARTBEAT_WINDOW" ]] || fail "kill-9 took ${elapsed}s to unblock (> ${HEARTBEAT_WINDOW}s heartbeat window)"

# --- (2) G11 multi-worker routing across hosts ------------------------------
# Bring the local llama worker back; the remote vLLM worker is still serving. Two
# workers on two hosts, each holding a different model → both must route 200, and
# when the local one leaves its model goes away while the remote keeps serving.
echo
echo "==> [criterion 2] G11 multi-worker routing across hosts"
start_llama_worker hostA-llama-3 "$REPO/atlas-fleet-worker-llama3.log"
LLAMA_PID=$LAST_WORKER_PID
for _ in $(seq 1 150); do
  [[ "$(ask "$LLAMACPP_MODEL")" == "200" && "$(ask "$VLLM_MODEL")" == "200" ]] && break
  sleep 2
done
A=$(ask "$LLAMACPP_MODEL"); B=$(ask "$VLLM_MODEL")
echo "    model A (llama, host A)=$A ; model B (vLLM, host B)=$B"
{ [[ "$A" == "200" && "$B" == "200" ]]; } || fail "both workers' models not routable (A=$A B=$B)"
echo "    -- local worker leaves: model A unavailable, model B keeps serving --"
kill -TERM "$LLAMA_PID"
for _ in $(seq 1 30); do [[ "$(ask "$LLAMACPP_MODEL")" != "200" ]] && break; sleep 1; done
A_GONE=$(ask "$LLAMACPP_MODEL"); B_LIVE=$(ask "$VLLM_MODEL")
echo "    after leave: model A=$A_GONE ; model B=$B_LIVE"
[[ "$A_GONE" != "200" ]] || fail "model A should be unavailable after its worker left (got $A_GONE)"
[[ "$B_LIVE" == "200" ]] || fail "model B (other host) should keep serving (got $B_LIVE)"

# --- aggregate the matrix (reuses the M0.5 capability generator) -------------
if [[ -f "$REPO/conformance/results/matrix-fleet.json" ]]; then
  echo
  echo "==> Generating capability matrix (fleet)"
  ( cd conformance && uv run --locked python capability_matrix.py \
      results/matrix-fleet.json \
      --output results/capability-matrix.json \
      --markdown results/CAPABILITY.md ) || true
fi

echo
if (( overall == 0 )); then
  echo "==> FLEET ACCEPTANCE GREEN — G1–G14 across two hosts, two engines"
else
  echo "==> FLEET ACCEPTANCE had failures (see ::error:: lines above)" >&2
fi
exit $overall
