.PHONY: build test lint fmt tidy snapshot clean

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

clean:
	rm -rf bin dist
