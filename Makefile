GO       ?= go
FUZZTIME ?= 30s

.PHONY: all build test cover fuzz lint vuln snapshot check clean e2e-firecracker e2e-helper deploy-runner

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

# End-to-end tests of the production path on a host installed with
# deploy-runner: as hcc-runner, through hcc-helper, with the installed
# image. The daemon is stopped meanwhile (it would share VM identities).
e2e-helper:
	@test -n "$(E2E_HOST)" || { echo "set E2E_HOST"; exit 1; }
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) test -c -tags e2e -o bin/fc-e2e.test ./internal/sandbox/firecracker
	scp -q bin/fc-e2e.test $(E2E_HOST):fc-e2e.test
	ssh $(E2E_HOST) 'set -u; t=$$(mktemp -d /var/tmp/hcc-e2e.XXXXXX); chmod 0755 $$t; install -m 0755 fc-e2e.test $$t/; rm -f fc-e2e.test; \
		sudo systemctl stop hcc-runner; \
		cd $$t && sudo -u hcc-runner HCC_E2E_CONFIG=/etc/hcc/runner.yaml ./fc-e2e.test -test.run E2E -test.v -test.timeout 5m; rc=$$?; \
		cd / && rm -rf $$t; sudo systemctl start hcc-runner; exit $$rc'

# Install or upgrade the runner daemon on a Linux/KVM host, e.g.
# make deploy-runner HOST=hcc-dev TARGET=server:hcc-dev-1
HOST   ?= $(E2E_HOST)
TARGET ?=
VERSION_PKG = github.com/first-it-consulting/harness-control-center/internal/version
deploy-runner:
	@test -n "$(HOST)" -a -n "$(TARGET)" || { echo "set HOST and TARGET"; exit 1; }
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags "-s -w -X $(VERSION_PKG).Version=$$(git describe --tags --always --dirty) -X $(VERSION_PKG).Commit=$$(git rev-parse --short HEAD)" \
		-o bin/hcc-linux-amd64 ./cmd/hcc
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/hcc-helper-linux-amd64 ./cmd/hcc-helper
	ssh $(HOST) 'rm -rf hcc-install && mkdir -m 0700 hcc-install'
	scp -q bin/hcc-linux-amd64 bin/hcc-helper-linux-amd64 deploy/install-runner.sh deploy/hcc-runner.service $(HOST):hcc-install/
	ssh $(HOST) 'cd hcc-install && ./install-runner.sh ./hcc-linux-amd64 ./hcc-helper-linux-amd64 $(E2E_ASSETS) $(TARGET); rc=$$?; cd && rm -rf hcc-install; exit $$rc'

snapshot:
	goreleaser release --snapshot --clean

# Everything CI checks, runnable locally before pushing.
check: lint test vuln

clean:
	rm -rf bin dist coverage.out
