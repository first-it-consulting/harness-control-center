# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Harness Control Center (`hcc`): a Control Center (Go backend + embedded web UI) defines *harnesses* (prompt, model, permissions, sandbox, budget) and runs them on *runners*, where each agent runs in its own Firecracker microVM. Agents act unattended, so the project's bar is safety first: deny by default, least privilege, every grant visible in `hcc explain`.

## Commands

```sh
make build                 # bin/hcc
make test                  # go test -race ./...
make lint                  # golangci-lint v2 (gofmt + goimports, gosec, revive, errorlint …)
make check                 # lint + test + govulncheck — what CI runs
make fuzz                  # every fuzz target, 30s each (FUZZTIME=5m for longer)
go test ./internal/policy -run TestExplain -v     # one test
go test ./internal/spec -run='^$' -fuzz=FuzzParse  # one fuzz target
```

CI runs tests on Linux, macOS and Windows; code that touches file modes, sockets or paths must work (or skip explicitly) on all three.

Real-VM tests need a Linux/KVM runner host (the dev host is `hcc-dev`, reached over ssh):

```sh
make deploy-runner HOST=hcc-dev TARGET=server:hcc-dev   # build linux binaries, install runner + images
make e2e E2E_HOST=hcc-dev                                # e2e build tag, real Firecracker
bin/hcc control serve                                    # the web UI on 127.0.0.1:7878 (prints a login link)
bin/hcc run --ssh hcc-dev --yes <harness.yaml>           # run a harness on the runner and follow it
```

On a Mac (Apple Silicon), the `vz` provider's kernel and arm64 images are built in Docker, and its real-VM tests need an ad-hoc signed test binary (the virtualization entitlement):

```sh
hack/vz/build-images.sh        # → ~/Library/Application Support/hcc/local/images
make e2e-vz                    # real VMs; signs test binaries via hack/vz/codesign-exec.sh
make build                     # on macOS also signs bin/hcc with hack/vz/entitlements.plist
```

`deploy-runner` rebuilds the VM images when their inputs change (`deploy/images/**`, `install-runner.sh`); a change to `deploy/images/claude-code/init` only takes effect after a redeploy.

## Architecture

Three binaries (`cmd/`):
- **`hcc`** — CLI, Control Center (`hcc control serve`, `hcc control mcp`) and runner daemon (`hcc runner`), all one binary.
- **`hcc-helper`** — the *only* code that runs as root (via one sudoers rule). Starts/kills/removes jails, builds the jailer command from its own root-owned config, re-validates every argument. Keep it tiny.
- **`hcc-guest`** — runs inside VMs (MCP plugin shim, A2A client).

How a run flows:
1. `internal/spec` parses a harness strictly (unknown fields are errors), applies defaults, validates bounds.
2. `internal/policy` is the **single enforcement point**: permission decisions, approval-gate matching, risk rating and the plain-language explanation. Other packages ask the engine; they never re-interpret the spec.
3. The Control Center (`internal/control`) reaches runners only by initiating: over ssh it runs `hcc runner stdio` and speaks the runner's HTTP/JSON API (`internal/runnerd`) on its owner-only Unix socket. Runners listen on no network port. (ADR-0003 and the architecture diagram still say gRPC; the code is HTTP/JSON.)
4. `internal/runner` owns the job state machine (`internal/runner/job.go` lists the allowed transitions), queue, concurrency, kill switch, schedules/loops, subagents, the contact lease, and builds the *task* JSON handed to the guest.
5. `internal/sandbox/firecracker` boots the VM through `hcc-helper` (Linux). On macOS, `internal/sandbox/vz` boots it with Apple's Virtualization framework, unprivileged, with no network device: the proxy is reached over vsock, forwarded in the guest by `hcc-guest vsock-proxy` (ADR-0016). Both providers share the guest contract (drive order, `hcc.*` kernel args, console markers), so the same images and inits work on both. The guest's `init` (`deploy/images/<image>/init`) reads the task, sets up the agent and prints result markers on the console.
6. The VM's only way out is a per-job egress proxy (`internal/egress`): CONNECT-only, allowlist by name and again by resolved IP, credential broker (secrets live on the runner in `/etc/hcc/credentials.yaml`, never in the VM), token metering against the budget (`internal/budget`), and approval gates (TLS inspection with a per-job CA when gates exist). There is no ICMP/raw network in a sandbox, and egress rules accept host names only, never IPs.
7. Results come back as a tar archive that `internal/results` re-packs into a safe one.

Other pieces: plugins run each in their own VM with their manifest's permissions (`internal/plugins`, ADR-0013); MCP relay and strict JSON for gate inspection (`internal/mcp`, ADR-0014); A2A (ADR-0015); tool catalogs of signed Alpine packages per image (`internal/tools`, ADR-0010); `internal/safetext` neutralizes agent-written text before it reaches a terminal or the UI.

Web UI (`internal/control/web`, embedded with `go:embed`): plain ES modules, no build step, no dependencies. The CSP forbids inline scripts/styles, and `TestWebNeverParsesHTML` fails on `innerHTML` and similar — build DOM with the `h()` helper (text and attributes only), because job output, errors and names come from agents.

## Rules for changes (from CONTRIBUTING.md)

- **Deny by default.** A new capability starts disabled and must appear in `hcc explain` (`internal/policy/explain.go`) and in the risk rating where it adds risk.
- Tests come with the code, including the denied and failure paths. Security-relevant parsing/matching code gets a fuzz target (add it to `make fuzz`).
- No secrets in sandboxes, logs or errors.
- Big decisions get an ADR in `docs/adr/`; a change to the threat model updates `docs/threat-model.md` in the same commit.
- Every change goes through a pull request on its own branch and is merged only with green CI; never push to `main`. Merging the release-please PR is what releases.
- Conventional Commits (`feat:`, `fix:`, `docs:` …): release-please builds releases and the changelog from them. Releases are tagged plain `vX.Y.Z` and built by GoReleaser (signed checksums, SBOMs).
- `examples/` harnesses are tested to stay valid.
