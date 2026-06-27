#!/usr/bin/env bash
# conformance-m3.sh — the per-PR M3 conformance tier (docs/internal/m3-acceptance.md).
#
# Drives the four M3 conformance groups against a real two-process llama.cpp
# deployment (CPU, no special hardware) plus a stub upstream for cloud-fallback:
#
#   G19 — prefix/session-affinity routing: two chat replicas; repeated same-prefix
#         requests land affine (hit), an x-atlas-session pin sticks, and the
#         affinity hit + warm-key series show up in /metrics.
#   G20 — embeddings + reranker model classes: /v1/embeddings returns vectors,
#         /v1/rerank orders documents, and a wrong-class request is rejected 400.
#   G21 — audit log: control-plane mutations (deploy/stop, key create/revoke) land
#         in the audit log with actor/action/target, append-only + durable across a
#         server restart, listable via `atlas audit` and GET /admin/audit.
#   G22 — cloud-fallback passthrough: with fallback on, an overflow (a model served
#         only upstream) spills to the stub and is labeled x-atlas-served-by: cloud
#         with cloud-class usage; with fallback off, the same request sheds locally.
#
# Mirrors the two-process gate steps in .github/workflows/ci.yml. Runs identically
# locally (validated on Apple Silicon) and on the CPU CI runner. Requires: go,
# python3, curl, jq.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

# --- config (env, with defaults) --------------------------------------------
ADDR=${ATLAS_ADDR:-127.0.0.1:9092}
API=http://$ADDR
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-m3}
JOIN_TOKEN=${ATLAS_JOIN_TOKEN:-m3-join-token}
export ATLAS_LOG_FILE=${ATLAS_LOG_FILE:-$REPO/atlas-m3-server.log}

CHAT_MODEL=${CONF_M3_CHAT:-qwen2.5-1.5b-instruct}
EMBED_MODEL=${CONF_M3_EMBED:-nomic-embed-text-v1.5}
RERANK_MODEL=${CONF_M3_RERANK:-bge-reranker-v2-m3}
SONNET_ALIAS=claude-sonnet-4-6

# Cloud-fallback: a model served by NO worker, so a request 404s locally and (with
# fallback configured) spills to the stub. provider=anthropic → stub /v1/messages.
CLOUD_MODEL=cloud-sonnet
NOFB_MODEL=no-fallback-model # undeployed + no fallback → sheds, never spills
STUB_PORT=${CONF_M3_STUB_PORT:-9145}
export UPSTREAM_KEY=stub-key-not-used

need() { command -v "$1" >/dev/null 2>&1 || { echo "conformance-m3: missing prerequisite '$1'" >&2; exit 2; }; }
need go; need python3; need curl; need jq

PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

fail() { echo "::error::$*" >&2; exit 1; }

echo "==> Building atlas"
go build -o "$REPO/atlas" ./cmd/atlas

echo "==> Provisioning admin API key"
export ATLAS_API_KEY=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" --admin --quiet)

echo "==> Pulling catalog models ($CHAT_MODEL, $EMBED_MODEL, $RERANK_MODEL)"
"$REPO/atlas" pull "$CHAT_MODEL" "$EMBED_MODEL" "$RERANK_MODEL"

# --- stub upstream for cloud-fallback (G22) ---------------------------------
# A tiny HTTP server that answers Anthropic /v1/messages with a canned reply
# carrying a recognizable marker + usage, so the gateway's spill is observable.
# Trailing X's with no suffix: portable across GNU (Linux/CI) and BSD (macOS) mktemp.
STUB_PY=$(mktemp "${TMPDIR:-/tmp}/m3-stub-XXXXXX")
cat >"$STUB_PY" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        self.rfile.read(n)
        body = json.dumps({
            "id": "msg_stub", "type": "message", "role": "assistant",
            "model": "claude-3-5-sonnet-stub",
            "content": [{"type": "text", "text": "CLOUD_STUB_OK"}],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 7, "output_tokens": 3},
        }).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
echo "==> Starting stub upstream on :$STUB_PORT"
python3 "$STUB_PY" "$STUB_PORT" &
PIDS+=($!)
for _ in $(seq 1 20); do curl -sf -o /dev/null -X POST "http://127.0.0.1:$STUB_PORT/v1/messages" -d '{}' && break; sleep 0.5; done

# --- control plane + two workers --------------------------------------------
# Server: chat aliases resolve to the chat model; cloud-fallback configured for
# CLOUD_MODEL only (spec: local:provider:upstream:keyEnv:baseURL). NOFB_MODEL has
# no fallback. autostart off so an undeployed model 404s deterministically (the
# G22 overflow trigger) instead of auto-loading.
# start_server launches the control plane with the full config (chat aliases +
# cloud-fallback for CLOUD_MODEL). Used for the initial boot and for the G21
# restart, so the restart is faithful (the fallback config G22 needs survives it).
start_server() {
  "$REPO/atlas" server \
    --addr "$ADDR" --autostart-timeout 0 \
    --token "$JOIN_TOKEN" \
    --alias "${SONNET_ALIAS}=${CHAT_MODEL}" \
    --cloud-fallback "${CLOUD_MODEL}:anthropic:claude-3-5-sonnet-20241022:UPSTREAM_KEY:http://127.0.0.1:${STUB_PORT}" \
    "$@"
}

echo "==> Starting atlas server"
start_server >"$ATLAS_LOG_FILE" 2>&1 &
PIDS+=($!)

# Worker 1 serves chat + embedding + reranker (three llama.cpp engines); worker 2
# serves a second chat replica so affinity (G19) has two replicas to choose from.
echo "==> Starting worker 1 (chat + embed + rerank)"
"$REPO/atlas" worker --join "ws://${ADDR}/workers/connect" --token "$JOIN_TOKEN" \
  --name w1 --model "$CHAT_MODEL" --model "$EMBED_MODEL" --model "$RERANK_MODEL" \
  >"$REPO/atlas-m3-w1.log" 2>&1 &
PIDS+=($!)
echo "==> Starting worker 2 (chat replica)"
"$REPO/atlas" worker --join "ws://${ADDR}/workers/connect" --token "$JOIN_TOKEN" \
  --name w2 --model "$CHAT_MODEL" \
  >"$REPO/atlas-m3-w2.log" 2>&1 &
PIDS+=($!)

api() { curl -sS -m 60 -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' "$@"; }
code() { curl -sS -m 60 -o /dev/null -w '%{http_code}' -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' "$@" || true; }

echo "==> Waiting for chat alias + embed + rerank to be routable"
ready=0
for _ in $(seq 1 180); do
  c=$(code "$API/v1/messages" -d "$(printf '{"model":"%s","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' "$SONNET_ALIAS")")
  e=$(code "$API/v1/embeddings" -d "$(printf '{"model":"%s","input":"ping"}' "$EMBED_MODEL")")
  r=$(code "$API/v1/rerank" -d "$(printf '{"model":"%s","query":"q","documents":["a"]}' "$RERANK_MODEL")")
  if [ "$c" = 200 ] && [ "$e" = 200 ] && [ "$r" = 200 ]; then ready=1; break; fi
  sleep 2
done
if [ "$ready" != 1 ]; then
  echo "=== server ==="; cat "$ATLAS_LOG_FILE"; echo "=== w1 ==="; cat "$REPO/atlas-m3-w1.log"; echo "=== w2 ==="; cat "$REPO/atlas-m3-w2.log"
  fail "two-process M3 deployment did not become ready (chat=$c embed=$e rerank=$r)"
fi
echo "==> Ready"

# =============================================================================
# G19 — prefix/session-affinity routing
# =============================================================================
echo
echo "=== G19: prefix/session-affinity routing ==="
chat_req() { # $1=session-header(optional) ; same prefix every call → affine
  local sess=()
  [ -n "${1:-}" ] && sess=(-H "x-atlas-session: $1")
  curl -sS -m 60 -o /dev/null -w '%{http_code}' \
    -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' "${sess[@]}" \
    "$API/v1/messages" \
    -d "$(printf '{"model":"%s","max_tokens":8,"messages":[{"role":"user","content":"You are a helpful assistant. What is 2+2?"}]}' "$SONNET_ALIAS")" || true
}
# A burst of identical-prefix requests: with two idle replicas, the affine
# (rendezvous-hashed) replica is always within tolerance, so each is an affinity hit.
for _ in $(seq 1 10); do chat_req >/dev/null; done
# A pinned session header routes by that key.
for _ in $(seq 1 5); do chat_req "agent-session-42" >/dev/null; done

METRICS=$(api "$API/metrics")
HITS=$(printf '%s' "$METRICS" | awk '/^atlas_affinity_total/ && /result="hit"/ {s+=$NF} END{print s+0}')
WARM=$(printf '%s' "$METRICS" | awk '/^atlas_affinity_warm_keys/ {s+=$NF} END{print s+0}')
echo "affinity hits=$HITS warm_keys(sum)=$WARM"
printf '%s' "$METRICS" | grep -qE '^atlas_affinity_total' || fail "G19: atlas_affinity_total series absent from /metrics"
printf '%s' "$METRICS" | grep -qE '^atlas_affinity_warm_keys' || fail "G19: atlas_affinity_warm_keys series absent from /metrics"
[ "$HITS" -ge 1 ] || fail "G19: expected affinity hits across repeated same-prefix requests, got $HITS"
awk "BEGIN{exit !($WARM >= 1)}" || fail "G19: expected a populated warm-key gauge, got $WARM"
echo "G19 passed: affinity hits accrue and hit/warm-key series are exposed."

# =============================================================================
# G20 — embeddings + reranker model classes
# =============================================================================
echo
echo "=== G20: embeddings + reranker model classes ==="
EMB=$(api "$API/v1/embeddings" -d "$(printf '{"model":"%s","input":["the quick brown fox","hello world"]}' "$EMBED_MODEL")")
N=$(printf '%s' "$EMB" | jq '.data | length')
DIM=$(printf '%s' "$EMB" | jq '.data[0].embedding | length')
echo "embeddings: $N vector(s), dim=$DIM"
[ "$N" = 2 ] || fail "G20: expected 2 embedding vectors, got $N ($EMB)"
[ "$DIM" -gt 0 ] || fail "G20: empty embedding vector ($EMB)"

RR=$(api "$API/v1/rerank" -d "$(printf '{"model":"%s","query":"What is the capital of France?","documents":["Bananas are a yellow fruit.","Paris is the capital of France.","The sky is blue."],"return_documents":true}' "$RERANK_MODEL")")
TOP_IDX=$(printf '%s' "$RR" | jq '.results[0].index')
ORDERED=$(printf '%s' "$RR" | jq '[.results[].relevance_score] | . == (sort | reverse)')
echo "rerank: top index=$TOP_IDX ordered_desc=$ORDERED results=$(printf '%s' "$RR" | jq -c '[.results[].index]')"
[ "$ORDERED" = true ] || fail "G20: rerank results not ordered by descending relevance_score ($RR)"
[ "$TOP_IDX" = 1 ] || fail "G20: expected the France document (index 1) ranked first, got $TOP_IDX ($RR)"

WRONG_EMB=$(code "$API/v1/embeddings" -d "$(printf '{"model":"%s","input":"x"}' "$SONNET_ALIAS")")
WRONG_RR=$(code "$API/v1/rerank" -d "$(printf '{"model":"%s","query":"q","documents":["a"]}' "$EMBED_MODEL")")
echo "wrong-class: embeddings@chat=$WRONG_EMB rerank@embedding=$WRONG_RR"
[ "$WRONG_EMB" = 400 ] || fail "G20: embeddings against a chat model should be 400, got $WRONG_EMB"
[ "$WRONG_RR" = 400 ] || fail "G20: rerank against an embedding model should be 400, got $WRONG_RR"
echo "G20 passed: embeddings + rerank serve their classes; wrong-class rejected 400."

# =============================================================================
# G21 — audit log
# =============================================================================
echo
echo "=== G21: audit log ==="
# Control-plane mutations: deploy + stop (HTTP admin, actor=admin key id), and key
# create + revoke (local CLI, actor=cli).
"$REPO/atlas" deploy "$CHAT_MODEL" --server "$API" >/dev/null || fail "G21: atlas deploy failed"
"$REPO/atlas" stop "$CHAT_MODEL" --server "$API" >/dev/null || fail "G21: atlas stop failed"
# Non-quiet create prints "Created key <ID>"; parse the ID so we can revoke it.
KEY_OUT=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" 2>&1)
NEWKEY_ID=$(printf '%s' "$KEY_OUT" | sed -n 's/^Created key \(.*\)$/\1/p' | head -1)
[ -n "$NEWKEY_ID" ] || fail "G21: could not parse new key id from: $KEY_OUT"
"$REPO/atlas" keys revoke "$NEWKEY_ID" --state-dir "$ATLAS_STATE_DIR" >/dev/null 2>&1 || fail "G21: atlas keys revoke failed"

AUDIT=$("$REPO/atlas" audit --state-dir "$ATLAS_STATE_DIR" --json)
have_action() { printf '%s' "$AUDIT" | jq -e --arg a "$1" 'any(.[]; .action == $a)' >/dev/null; }
for action in deployment.set deployment.stop key.create key.revoke; do
  have_action "$action" || fail "G21: audit log missing action '$action' ($(printf '%s' "$AUDIT" | jq -c '[.[].action]'))"
done
# deployment.set must carry the chat model as target and a non-empty actor.
printf '%s' "$AUDIT" | jq -e --arg m "$CHAT_MODEL" 'any(.[]; .action=="deployment.set" and .target==$m and (.actor|length>0))' >/dev/null \
  || fail "G21: deployment.set record missing target/actor ($(printf '%s' "$AUDIT" | jq -c '.[0]'))"
# The HTTP read API returns the same trail.
ADMIN_AUDIT=$(api "$API/admin/audit?action=deployment.set")
printf '%s' "$ADMIN_AUDIT" | jq -e 'type=="array" and (length>=1)' >/dev/null \
  || fail "G21: GET /admin/audit did not return deployment.set records ($ADMIN_AUDIT)"
echo "audit actions present: $(printf '%s' "$AUDIT" | jq -c '[.[].action] | unique')"

# Append-only + durable: restart the server, the trail survives.
BEFORE=$(printf '%s' "$AUDIT" | jq 'length')
SRV_PID=${PIDS[1]} # server is the 2nd PID started (after the stub)
kill "$SRV_PID" 2>/dev/null || true
sleep 2
start_server >>"$ATLAS_LOG_FILE" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 30); do curl -sf -o /dev/null "$API/readyz" 2>/dev/null && break; sleep 1; done
AFTER=$("$REPO/atlas" audit --state-dir "$ATLAS_STATE_DIR" --json | jq 'length')
echo "audit rows before restart=$BEFORE after restart=$AFTER"
[ "$AFTER" -ge "$BEFORE" ] || fail "G21: audit trail not durable across restart (before=$BEFORE after=$AFTER)"
echo "G21 passed: mutations audited with actor/action/target, durable across restart."

# =============================================================================
# G22 — cloud-fallback passthrough
# =============================================================================
echo
echo "=== G22: cloud-fallback passthrough ==="
# served_by extracts the x-atlas-served-by header value (empty if absent). The
# `|| true` keeps a no-match grep from tripping `set -e`.
served_by() { grep -i '^x-atlas-served-by:' "$1" | tr -d '\r' | awk '{print $2}' || true; }

# Fallback ON: CLOUD_MODEL is served by no worker → 404 locally → spills to stub.
HDRS=$(mktemp); BODY=$(mktemp)
ON_CODE=$(curl -sS -m 60 -D "$HDRS" -o "$BODY" -w '%{http_code}' \
  -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
  "$API/v1/messages" \
  -d "$(printf '{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' "$CLOUD_MODEL")" || true)
SERVED_BY=$(served_by "$HDRS")
echo "fallback ON: code=$ON_CODE served-by=${SERVED_BY:-<none>} body=$(head -c 160 "$BODY")"
if [ "$ON_CODE" != 200 ] || [ "$SERVED_BY" != cloud ]; then
  echo "--- response headers ---"; cat "$HDRS"
  echo "--- server log tail ---"; tail -30 "$ATLAS_LOG_FILE"
fi
[ "$ON_CODE" = 200 ] || fail "G22: spilled request should return 200 from the stub, got $ON_CODE ($(cat "$BODY"))"
[ "$SERVED_BY" = cloud ] || fail "G22: spilled response missing x-atlas-served-by: cloud (got '$SERVED_BY')"
grep -q CLOUD_STUB_OK "$BODY" || fail "G22: response body is not the upstream stub's reply ($(cat "$BODY"))"

# Cloud-served tokens are attributed to a distinct cloud ledger class. The async
# usage writer batches rows and flushes every ~250ms, and `atlas usage` reads the
# store from a separate process, so poll briefly for the cloud row to appear.
CLOUD_ROWS=0
for _ in $(seq 1 20); do
  USAGE=$("$REPO/atlas" usage --state-dir "$ATLAS_STATE_DIR" --json)
  CLOUD_ROWS=$(printf '%s' "$USAGE" | jq '[.by_worker[].group | select(startswith("cloud:"))] | length')
  [ "$CLOUD_ROWS" -ge 1 ] && break
  sleep 1
done
echo "cloud-class usage rows=$CLOUD_ROWS by_worker=$(printf '%s' "$USAGE" | jq -c '[.by_worker[].group]')"
[ "$CLOUD_ROWS" -ge 1 ] || fail "G22: no usage attributed to the cloud ledger class ($USAGE)"

# Fallback OFF: NOFB_MODEL has no fallback configured → sheds locally, never cloud.
OFF_HDRS=$(mktemp)
OFF_CODE=$(curl -sS -m 60 -D "$OFF_HDRS" -o /dev/null -w '%{http_code}' \
  -H "x-api-key: $ATLAS_API_KEY" -H 'content-type: application/json' \
  "$API/v1/messages" \
  -d "$(printf '{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' "$NOFB_MODEL")" || true)
OFF_SERVED=$(served_by "$OFF_HDRS")
echo "fallback OFF: code=$OFF_CODE served-by=${OFF_SERVED:-<none>}"
[ "$OFF_CODE" != 200 ] || fail "G22: a model with no fallback should not be served 200 ($OFF_CODE)"
[ "$OFF_SERVED" != cloud ] || fail "G22: a model with no fallback must not be labeled cloud-served"
echo "G22 passed: overflow spills + labels cloud-served with cloud usage; off-path sheds locally."

echo
echo "==> M3 conformance GREEN: G19, G20, G21, G22"
