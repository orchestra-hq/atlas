#!/usr/bin/env bash
# conformance-m8.sh — the per-PR M8 conformance tier (docs/internal/m8-acceptance.md).
#
# Proves ADR-0015's headline promise end to end on a real engine: `atlas up
# --model <hugging-face-repo>` for a known-family repo that is NOT in the starter
# catalog auto-configures from the model's own metadata and serves an agent-grade
# endpoint. The single conformance group:
#
#   G23 — bring-any-model auto-config: `atlas up --model <repo>` (a known-family
#         GGUF repo with no catalog row) resolves through resolveRaw → Classify,
#         prints the `Auto-configured … family` line (NOT the bare passthrough,
#         NOT the unknown-family plain-chat warning), and the resulting endpoint
#         passes the agent-critical conformance gates — G3 (tool loop) and G9 (the
#         streamed >=3-call agent-SDK loop) — plus G1/G2 substrate and G4 reasoning.
#
# Mirrors scripts/conformance-m3.sh and the single-node `Conformance (llama.cpp)`
# job in .github/workflows/ci.yml. CPU-only; GPU-engine breadth (parser-flag
# families on vLLM/SGLang, auto-config on MLX) is the nightly tier. The harness
# (conformance/run.py) is model-agnostic, so this adds a driver, not harness code.
# Requires: go, uv, npm, curl.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$PWD

# --- config (env, with defaults) --------------------------------------------
ADDR=${ATLAS_ADDR:-127.0.0.1:9093}
API=http://$ADDR
export ATLAS_STATE_DIR=${ATLAS_STATE_DIR:-$REPO/.atlas-m8}
export ATLAS_LOG_FILE=${ATLAS_LOG_FILE:-$REPO/atlas-m8.log}
# Pin llama.cpp's -hf download cache to a known dir under the state dir (the engine
# subprocess inherits this env) so CI can cache it keyed on the repo — only a cold
# cache pays the Hugging Face download.
export LLAMA_CACHE=${LLAMA_CACHE:-$ATLAS_STATE_DIR/llama-cache}

# A KNOWN-FAMILY GGUF repo that is NOT a catalog name (catalog names are tokens
# like `qwen3-0.6b`, so cat.Lookup misses and this flows through resolveRaw — the
# auto-config path under test). Qwen-published official GGUF: GGUF
# general.architecture=qwen3 → modelmeta normalizes to the known `qwen3` family
# (Reasoning: true), ~0.4 GiB at Q4_K_M — well within a CPU runner. Reasoning-
# capable so G4 exercises the auto-configured reasoning gating, not just chat.
MODEL=${CONF_M8_MODEL:-Qwen/Qwen3-0.6B-GGUF}

need() { command -v "$1" >/dev/null 2>&1 || { echo "conformance-m8: missing prerequisite '$1'" >&2; exit 2; }; }
need go; need uv; need npm; need curl

PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

fail() {
  echo "::error::$*" >&2
  echo "=== atlas log tail ==="; tail -60 "$ATLAS_LOG_FILE" 2>/dev/null || true
  exit 1
}

echo "==> Building atlas"
go build -o "$REPO/atlas" ./cmd/atlas

echo "==> Provisioning API key"
export ATLAS_API_KEY=$("$REPO/atlas" keys create --state-dir "$ATLAS_STATE_DIR" --quiet)

# --- boot the auto-configured model -----------------------------------------
# `atlas up --model <repo>` with no catalog entry for the repo: resolveRaw
# inspects the repo's GGUF header, Classify maps qwen3 → the known family, and the
# full serving plan (reasoning gating, sampling, context) is applied. The llama.cpp
# served name for an -hf spec is the repo id itself, so the harness addresses the
# model by that name. Aliases let G-groups that want a tier name resolve it.
echo "==> Starting atlas up --model $MODEL (auto-config, no catalog row)"
"$REPO/atlas" up \
  --model "$MODEL" \
  --alias "claude-sonnet-4-6=${MODEL}" \
  --alias "claude-haiku-4-5=${MODEL}" \
  --alias "claude-opus-4-1=${MODEL}" \
  --addr "$ADDR" >"$ATLAS_LOG_FILE" 2>&1 &
PIDS+=($!)

echo "==> Waiting for the auto-configured model to be ready"
ready=0
for _ in $(seq 1 240); do
  curl -sf -o /dev/null "$API/readyz" && { ready=1; break; }
  sleep 2
done
[ "$ready" = 1 ] || fail "G23: atlas up did not become ready for $MODEL"
echo "==> Ready"

# =============================================================================
# G23 — bring-any-model auto-config
# =============================================================================
echo
echo "=== G23: bring-any-model auto-config ==="
# The distinguishing claim: the repo was AUTO-CONFIGURED from its family metadata,
# not served as a bare passthrough and not flagged as an unknown family. The
# `Auto-configured … family` line is resolveRaw's machine-checkable signal it took
# the Classify path; the plain-chat warning is the unknown-family middle case.
grep -q "Auto-configured \"$MODEL\" as the .* family" "$ATLAS_LOG_FILE" \
  || fail "G23: no auto-config line for $MODEL — it fell through to the bare passthrough (auto-config regressed)"
if grep -q "warning: serving $MODEL as plain chat" "$ATLAS_LOG_FILE"; then
  fail "G23: $MODEL was served as an UNKNOWN family (plain-chat warning) — expected a known family"
fi
echo "auto-config confirmed: $(grep -m1 'Auto-configured' "$ATLAS_LOG_FILE")"

# Now prove the auto-configured endpoint is agent-grade: drive the agent-critical
# conformance gates (G3 tool loop, G9 streamed agent-SDK loop) plus G1/G2 substrate
# and G4 reasoning against it via the Anthropic Python SDK + agent-SDK suites. The
# repo id is both the chat and reasoning model (qwen3 is reasoning-capable); the
# genuinely-non-reasoning G4 case is omitted (ATLAS_NONREASONING_MODEL unset →
# skipped), as on the single-engine tiers.
#
# --skip-ts: the required groups are fully covered by pytest (Python SDK + agent
# SDK); the anthropic-ts subset adds no auto-config-specific signal (the wire format
# is identical however the model was configured, and the TS SDK is already proven
# per-engine by the catalog conformance jobs). It is also the slow long-pole — its
# G4 streamed-thinking case (up to 2048 tokens) can blow the vitest timeout on a
# cold CPU runner serving a tiny reasoning model — so skipping it keeps the gate
# fast and reliable without weakening the auto-config claim.
echo "==> Running the conformance harness against the auto-configured endpoint"
( cd conformance && uv run --locked python run.py \
    --base-url "$API" \
    --engine llamacpp \
    --model "$MODEL" \
    --reasoning-model "$MODEL" \
    --skip-ts \
    --require G1,G2,G3,G4,G9 ) \
  || fail "G23: the auto-configured endpoint did not pass the agent-critical gates (G1,G2,G3,G4,G9)"

echo
echo "==> M8 conformance GREEN: G23 (auto-config serves agent-grade for a catalog-less known-family repo)"
