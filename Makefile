GO       ?= go
FUZZTIME ?= 30s

.PHONY: all build test cover fuzz lint vuln snapshot check clean e2e-firecracker

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
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# End-to-end tests against real Firecracker on a remote Linux/KVM host,
# e.g. make e2e-firecracker E2E_HOST=hcc-dev. The host needs the assets
# from hack/firecracker-poc/run.sh and non-interactive sudo.
E2E_HOST   ?=
E2E_ASSETS ?= ~/.cache/hcc-poc
e2e-firecracker:
	@test -n "$(E2E_HOST)" || { echo "set E2E_HOST"; exit 1; }
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) test -c -tags e2e -o bin/fc-e2e.test ./internal/sandbox/firecracker
	scp -q bin/fc-e2e.test $(E2E_HOST):fc-e2e.test
	ssh $(E2E_HOST) 'HCC_E2E_ASSETS=$(E2E_ASSETS) ./fc-e2e.test -test.run E2E -test.v -test.timeout 5m; rc=$$?; rm -f fc-e2e.test; exit $$rc'

snapshot:
	goreleaser release --snapshot --clean

# Everything CI checks, runnable locally before pushing.
check: lint test vuln

clean:
	rm -rf bin dist coverage.out
