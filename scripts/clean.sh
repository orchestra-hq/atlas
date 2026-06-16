#!/usr/bin/env bash
# clean.sh — stop stray local e2e processes and remove ephemeral artifacts.
#
# Safe to run anytime: it touches nothing under version control and nothing
# expensive to recreate. The downloaded model weights (models/) and the
# provisioned engine runtime (under the Atlas state dir) are deliberately left
# in place — re-downloading them costs ~1 GB and several minutes.
#
# Adjust the two lists below if the e2e process names or artifact paths change.
set -euo pipefail

cd "$(dirname "$0")/.."

# Process name patterns to terminate (matched with `pkill -f`).
PROCESS_PATTERNS=(
  "atlas up"
  "llama-server"
)

# Paths (relative to the repo root) to delete.
ARTIFACT_PATHS=(
  "atlas"               # locally built binary (CI-style `go build -o atlas`)
  "atlas.log"           # `atlas up` log captured during an e2e run
  "conformance/results" # harness output (matrix.json + per-suite json)
)

echo "==> Stopping local e2e processes"
for pat in "${PROCESS_PATTERNS[@]}"; do
  if pkill -f "$pat" 2>/dev/null; then
    echo "    killed:       $pat"
  else
    echo "    not running:  $pat"
  fi
done

echo "==> Removing ephemeral artifacts"
for path in "${ARTIFACT_PATHS[@]}"; do
  if [ -e "$path" ]; then
    rm -rf "$path"
    echo "    removed:      $path"
  fi
done

# bin/ and dist/ are owned by `make clean`.
make clean >/dev/null
echo "    removed:      bin dist (make clean)"

echo "==> Clean."
