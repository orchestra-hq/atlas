.PHONY: build test lint fmt tidy snapshot conformance clean check ship

build:
	go build -o bin/atlas ./cmd/atlas

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

tidy:
	go mod tidy

# Full local release dry-run: cross-platform builds + archives into dist/.
snapshot:
	goreleaser release --snapshot --clean

# Conformance harness (needs uv + node): runs against the built-in stub
# gateway and gates on G1; the gate widens as m0-build-plan phases land.
conformance:
	cd conformance/ts && npm install --no-fund --no-audit --loglevel=error
	cd conformance && uv run python run.py --require G1

clean:
	rm -rf bin dist

# Local pre-flight: format + vet + lint + test + tidy (the gates CI runs).
check:
	@bash scripts/check.sh

# One command from working changes to a squash-merge into main: clean, check,
# commit, push, open PR, wait for CI, then sync + delete the branch.
#   make ship MSG="PR title" [BODY=path/to/body.md]
ship:
	@bash scripts/ship.sh "$(MSG)" $(BODY)
