#!/usr/bin/env bash
# check.sh — local pre-flight: auto-format, then run the same gates CI does.
#
# Formats Go and Markdown in place, then runs vet, lint, tests, and verifies
# go.mod/go.sum are tidy. Exits non-zero on the first failure. No git or
# network side effects — run it as often as you like during development.
#
# Formatting here means the eventual commit (and its pre-commit hook) is a
# no-op, so `ship.sh` never trips over an unformatted file.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Formatting Go (make fmt)"
make fmt

echo "==> Formatting Markdown"
# Tracked .md only — excludes node_modules and other gitignored trees. Matches
# the tools the pre-commit hook runs (.githooks/pre-commit). website/ is excluded:
# the docs site (Astro Starlight, MDX + directives) has its own build + link-check
# gate in .github/workflows/docs-site.yml and its own formatting conventions.
md_files="$(git ls-files '*.md' ':!:website/**')"
if [ -n "$md_files" ]; then
  printf '%s\n' "$md_files" | tr '\n' '\0' | xargs -0 npx --yes prettier --write --log-level warn
  printf '%s\n' "$md_files" | tr '\n' '\0' | xargs -0 npx --yes markdownlint-cli2 --fix
fi

echo "==> Lint GitHub workflows (actionlint)"
# Validates the workflow schema, job/expression references, and shellchecks the
# bash inside run: steps — far more than a bare YAML parse. Optional tool, so
# skip with a hint rather than failing if it isn't installed.
if command -v actionlint >/dev/null 2>&1; then
  actionlint
else
  echo "    actionlint not installed; skipping (brew install actionlint)"
fi

echo "==> go vet"
go vet ./...

echo "==> Lint (make lint)"
make lint

echo "==> Test (make test)"
make test

echo "==> Tidy check (go mod tidy)"
make tidy
if ! git diff --quiet -- go.mod go.sum; then
  echo "    go.mod/go.sum changed after tidy — commit the result:" >&2
  git --no-pager diff -- go.mod go.sum >&2
  exit 1
fi

echo "==> All checks passed."
