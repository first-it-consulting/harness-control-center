GO       ?= go
FUZZTIME ?= 30s

.PHONY: all build test cover fuzz lint vuln snapshot check clean

all: check build

build:
	$(GO) build -trimpath -o bin/hcc ./cmd/hcc

test:
	$(GO) test -race ./...

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fuzz:
	$(GO) test ./internal/spec -run='^$$' -fuzz=FuzzParse -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/policy -run='^$$' -fuzz=FuzzCheckEgress -fuzztime=$(FUZZTIME)

lint:
	golangci-lint run

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

snapshot:
	goreleaser release --snapshot --clean

# Everything CI checks, runnable locally before pushing.
check: lint test vuln

clean:
	rm -rf bin dist coverage.out
