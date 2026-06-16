#!/usr/bin/env bash
# ship.sh — take the current feature branch from working changes to a
# squash-merge into main, running the same gates CI does on the way. This is
# the repo's PR workflow (CLAUDE.md: PR required, CI green, squash-only) as a
# single command.
#
#   scripts/ship.sh "PR title" [body-file]
#
#   1. clean.sh  — stop e2e processes, remove ephemeral artifacts
#   2. check.sh  — format + vet + lint + test + tidy
#   3. commit any working changes, push the branch
#   4. open a PR (if none yet) and enable squash auto-merge
#   5. poll until every required check is green and the PR merges
#   6. sync local main, delete the feature branch, confirm cleanup
#
# The branch commit message is just the title; the squash commit that lands on
# main is built from the PR title + body, so pass a body-file when you want a
# richer description. Attribution trailers (Co-Authored-By, etc.) belong in
# that body-file, not hardcoded here — this script is run by humans and agents
# alike.
#
# Requires: gh (authenticated). Refuses to run on the base branch.
#
# Tunable via env: POLL_INTERVAL (s), POLL_TIMEOUT (s), BASE_BRANCH.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/.."

POLL_INTERVAL="${POLL_INTERVAL:-20}"
POLL_TIMEOUT="${POLL_TIMEOUT:-1800}"
BASE_BRANCH="${BASE_BRANCH:-main}"

title="${1:-}"
body_file="${2:-}"

if [ -z "$title" ]; then
  echo "usage: scripts/ship.sh \"PR title\" [body-file]" >&2
  exit 2
fi
command -v gh >/dev/null 2>&1 || { echo "ship: gh CLI not found" >&2; exit 2; }
if [ -n "$body_file" ] && [ ! -f "$body_file" ]; then
  echo "ship: body-file not found: $body_file" >&2
  exit 2
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
if [ "$branch" = "$BASE_BRANCH" ]; then
  echo "ship: refusing to ship from '$BASE_BRANCH' — switch to a feature branch first" >&2
  exit 2
fi

# 1 + 2: local prep and verification (fail fast before any remote change).
bash "$here/clean.sh"
bash "$here/check.sh"

# 3: commit any changes (check.sh may have reformatted files) and push.
if [ -n "$(git status --porcelain)" ]; then
  echo "==> Committing working changes"
  git add -A
  git commit -m "$title"
else
  echo "==> No working changes to commit"
fi

echo "==> Pushing $branch"
git push -u origin "$branch"

# 4: open the PR (idempotent) and arm squash auto-merge.
if gh pr view "$branch" >/dev/null 2>&1; then
  echo "==> PR already open for $branch"
else
  echo "==> Opening PR"
  if [ -n "$body_file" ]; then
    gh pr create --base "$BASE_BRANCH" --head "$branch" --title "$title" --body-file "$body_file"
  else
    gh pr create --base "$BASE_BRANCH" --head "$branch" --title "$title" --body "$title"
  fi
fi

echo "==> Enabling squash auto-merge"
gh pr merge "$branch" --squash --auto

# 5: poll until merged, surfacing a hard CI failure rather than waiting out the
# timeout.
echo "==> Waiting for CI (poll ${POLL_INTERVAL}s, timeout ${POLL_TIMEOUT}s)"
elapsed=0
while :; do
  state="$(gh pr view "$branch" --json state -q .state 2>/dev/null || echo UNKNOWN)"
  case "$state" in
    MERGED) echo "    merged."; break ;;
    CLOSED) echo "ship: PR was closed without merging" >&2; exit 1 ;;
  esac

  checks_out="$(gh pr checks "$branch" 2>/dev/null || true)"
  if printf '%s\n' "$checks_out" | awk -F'\t' '$2=="fail"{f=1} END{exit !f}'; then
    echo "ship: a required check failed:" >&2
    printf '%s\n' "$checks_out" >&2
    exit 1
  fi

  if [ "$elapsed" -ge "$POLL_TIMEOUT" ]; then
    echo "ship: timed out after ${POLL_TIMEOUT}s; current checks:" >&2
    printf '%s\n' "$checks_out" >&2
    exit 1
  fi
  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
done

# 6: sync main, delete the feature branch (local + best-effort remote), confirm.
echo "==> Syncing $BASE_BRANCH and removing $branch"
git checkout "$BASE_BRANCH"
git pull --ff-only
git branch -D "$branch"
git push origin --delete "$branch" 2>/dev/null || true

echo "==> Confirming cleanup"
bash "$here/clean.sh"
git status --short

echo "==> Shipped: $title"
