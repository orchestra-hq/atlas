#!/usr/bin/env bash
# nightly.sh — drive the nightly acceptance workflow (.github/workflows/nightly-gpu.yml)
# from one command instead of hand-running gh dispatch / list / watch / download / view.
#
#   scripts/nightly.sh run [tracks]   dispatch the workflow, watch it to completion,
#                                     then download + summarize the conformance matrix
#                                     (tracks: "gpu", "cpu", "fleet", or any combination;
#                                     default "gpu cpu" — "fleet" is the multi-host M1
#                                     run and is opt-in, e.g. `nightly.sh run fleet`)
#   scripts/nightly.sh watch [run]    watch a run to completion (default: latest)
#   scripts/nightly.sh logs  [run]    print the logs of failed steps (default: latest)
#   scripts/nightly.sh fetch [run]    download + summarize the conformance matrix (default: latest)
#   scripts/nightly.sh status         list recent runs (the default with no subcommand)
#
# A "run" argument is a run ID, or "latest" (the default). Artifacts land under
# .scratch/nightly/<run-id>/ (gitignored). The matrix summary surfaces each failing
# cell (group/criterion/client + message) and prints CAPABILITY.md when present —
# the whole point being to see *why* a nightly went red without spelunking the UI.
#
# Requires: gh (authenticated). jq is used for the matrix summary when available.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/.."

WORKFLOW="${WORKFLOW:-nightly-gpu.yml}"
POLL_INTERVAL="${POLL_INTERVAL:-15}"
DL_ROOT=".scratch/nightly"

command -v gh >/dev/null 2>&1 || { echo "nightly: gh CLI not found" >&2; exit 2; }

have_jq() { command -v jq >/dev/null 2>&1; }

latest_run_id() {
  gh run list --workflow "$WORKFLOW" -L 1 --json databaseId -q '.[0].databaseId' 2>/dev/null
}

# Resolve a run argument ("", "latest", or an explicit id) to a concrete run id.
resolve_run() {
  local arg="${1:-latest}"
  if [ "$arg" = "latest" ] || [ -z "$arg" ]; then
    local id
    id="$(latest_run_id)"
    [ -n "$id" ] || { echo "nightly: no runs found for $WORKFLOW" >&2; exit 1; }
    echo "$id"
  else
    echo "$arg"
  fi
}

# Summarize a downloaded conformance matrix: target, group totals, failing cells.
summarize_matrix() {
  local dir="$1"
  local found=0

  for cap in "$dir"/*/CAPABILITY.md "$dir"/CAPABILITY.md; do
    [ -f "$cap" ] || continue
    echo "==> $cap"
    cat "$cap"
    echo
  done

  while IFS= read -r m; do
    found=1
    echo "==> $m"
    if have_jq; then
      jq -r '
        "target: engine=\(.target.engine) model=\(.target.model) kind=\(.target.kind)",
        "summary: \(.summary.passed) pass / \(.summary.failed) fail / \(.summary.skipped) skip (of \(.summary.total))",
        (if (.summary.failed // 0) > 0 then
          "failing cells:",
          ( .cells[] | select(.status=="fail")
            | "  [\(.group)][c\(.criterion)][\(.client // "py")] \((.failure.message // "") | gsub("\n";" ") | .[0:160])" )
        else "  (no failing cells)" end)
      ' "$m"
    else
      echo "    (install jq for a parsed summary; raw file above)"
    fi
    echo
  done < <(find "$dir" -name 'matrix-*.json' -o -name 'matrix.json' 2>/dev/null | sort)

  [ "$found" -eq 1 ] || echo "    (no matrix-*.json found in $dir)"
}

cmd_status() {
  gh run list --workflow "$WORKFLOW" -L 10
}

cmd_watch() {
  local id; id="$(resolve_run "${1:-latest}")"
  echo "==> Watching run $id"
  # --exit-status makes gh return non-zero if the run concluded as a failure.
  gh run watch "$id" --interval "$POLL_INTERVAL" --exit-status
}

cmd_logs() {
  local id; id="$(resolve_run "${1:-latest}")"
  echo "==> Failed-step logs for run $id"
  gh run view "$id" --log-failed
}

cmd_fetch() {
  local id; id="$(resolve_run "${1:-latest}")"
  local dir="$DL_ROOT/$id"
  mkdir -p "$dir"
  echo "==> Downloading artifacts for run $id -> $dir"
  # Best-effort: a red run may have produced only one track's artifact, or none.
  gh run download "$id" -D "$dir" 2>/dev/null || echo "    (no artifacts available)"
  echo
  summarize_matrix "$dir"
}

cmd_run() {
  local tracks="${1:-gpu cpu}"
  local before; before="$(latest_run_id || true)"

  echo "==> Dispatching $WORKFLOW (tracks: $tracks)"
  gh workflow run "$WORKFLOW" -f tracks="$tracks"

  # gh workflow run doesn't return the run id; poll until a newer run appears.
  echo "==> Waiting for the run to register"
  local id="" tries=0
  while [ "$tries" -lt 24 ]; do
    id="$(latest_run_id || true)"
    [ -n "$id" ] && [ "$id" != "$before" ] && break
    sleep 5
    tries=$((tries + 1))
  done
  [ -n "$id" ] && [ "$id" != "$before" ] || { echo "nightly: dispatched but couldn't find the new run; try 'scripts/nightly.sh status'" >&2; exit 1; }
  echo "    run $id — $(gh run view "$id" --json url -q .url 2>/dev/null)"

  local rc=0
  cmd_watch "$id" || rc=$?
  echo
  cmd_fetch "$id"

  if [ "$rc" -ne 0 ]; then
    echo
    echo "==> Run concluded red; failed-step logs:"
    gh run view "$id" --log-failed || true
  fi
  return "$rc"
}

sub="${1:-status}"
[ "$#" -gt 0 ] && shift || true
case "$sub" in
  run)    cmd_run "${1:-}" ;;
  watch)  cmd_watch "${1:-latest}" ;;
  logs)   cmd_logs "${1:-latest}" ;;
  fetch)  cmd_fetch "${1:-latest}" ;;
  status) cmd_status ;;
  -h|--help|help)
    # Print the leading comment block (every #-line after the shebang).
    awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0" ;;
  *) echo "nightly: unknown subcommand '$sub' (try: run watch logs fetch status)" >&2; exit 2 ;;
esac
