# Architecture

## Components

```
 ┌──────────── your computer ─────────────┐        ┌──────── server (or local VM) ────────┐
 │                                        │        │                                      │
 │  Control Center (hcc control)          │  SSH   │  Runner (hcc runner, unprivileged)   │
 │  ┌──────────┐  ┌─────────────────────┐ │ tunnel │  ┌──────────┐  ┌──────────────────┐  │
 │  │ Web UI   │──│ API · harness store │─┼────────┼─▶│ scheduler│  │ event/log buffer │  │
 │  └──────────┘  │ secrets vault       │ │ (gRPC) │  └────┬─────┘  └──────────────────┘  │
 │                └─────────────────────┘ │        │       │ policy engine (one per run) │
 └────────────────────────────────────────┘        │  ┌────▼──────────────────────────┐   │
                                                   │  │ privileged helper (tiny)      │   │
                                                   │  │ tap devices · jailer · nft    │   │
                                                   │  └────┬──────────────────────────┘   │
                                                   │  ┌────▼──────────┐   ┌────────────┐  │
                                                   │  │ microVM       │──▶│ egress     │──┼─▶ allowlisted
                                                   │  │ agent runtime │   │ proxy +    │  │   hosts only
                                                   │  │ (no secrets)  │   │ cred broker│  │
                                                   │  └───────────────┘   └────────────┘  │
                                                   └──────────────────────────────────────┘
```

- **Control Center**: the Go backend and the web UI, shipped as one binary
  (`hcc`). It stores harnesses and secrets, installs and upgrades runners,
  and shows queues, logs and costs. It is **not needed at runtime**: runners
  keep working while your laptop sleeps.
- **Runner**: the same binary in runner mode, running as an unprivileged
  user. It owns scheduling, sandbox lifecycle, budgets, the kill switch, and
  buffering of events and logs until the Control Center reconnects.
- **Privileged helper**: the only component with elevated rights. It has a
  minimal, audited API: create or destroy a jailed microVM, its tap device
  and its firewall rules. Nothing else.
- **Sandbox**: one microVM per agent run (see
  [ADR-0001](adr/0001-sandbox-isolation.md)). Its only route out is the
  egress proxy.
- **Egress proxy and credential broker**: these enforce the allowlist
  (by name, then again by resolved IP), and add credentials to outbound
  requests. The agent never holds a secret.
- **Policy engine** (`internal/policy`): the single implementation of
  "is this allowed?", used by every enforcement point.

## Harness lifecycle

```
queued → provisioning → running → succeeded | failed | cancelled | timed_out | over_budget
```

(`awaiting_approval` between running and the end states is planned with approval gates.)

## Plugins

These are extension points (see [ADR-0002](adr/0002-plugin-system.md)):
`SandboxProvider`, `AgentRuntime`, `Transport`, `CredentialProvider`,
`ProtocolAdapter` (MCP, A2A) and `Notifier`.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/hcc` | the single binary |
| `internal/spec` | harness spec: types, defaults, strict parsing, validation |
| `internal/policy` | permission decisions, risk rating, plain-language explanation |
| `internal/sandbox` | provider-neutral sandbox interface and validated run spec |
| `internal/sandbox/firecracker` | Firecracker provider: jailer, cgroup v2 limits, kill switch, cleanup |
| `internal/runner` | job queue and state machine, concurrency limit, per-job cancel, kill switch, event stream, schedules and loops (ADR-0008) |
| `internal/cron` | 5-field cron expressions in UTC |
| `internal/helper`, `cmd/hcc-helper` | the only root-side code: start, kill, remove a jail, network links, copy out a jail's disk; validates every argument |
| `internal/budget` | per-job token and cost meter |
| `internal/results` | re-packs a guest's results archive into a safe one (ADR-0006) |
| `internal/egress` | per-VM HTTPS (CONNECT) egress proxy enforcing the harness's network policy (ADR-0004); credential broker with a per-job CA (ADR-0005) |
| `internal/credentials` | the broker's store: secrets bound to hosts and headers |
| `internal/control` | Control Center: loopback web UI (embedded, no dependencies) and JSON API, runner list, harness library (ADR-0007) |
| `internal/safetext` | shows agent-written text without letting it drive a terminal or disguise itself |
| `internal/runnerd` | runner daemon: HTTP API on an owner-only Unix socket, client (Unix socket or ssh), size-capped console logs, job store and results |
| `deploy/` | systemd unit and install script for runners |
| `hack/firecracker-poc` | standalone proof of concept and e2e test assets |
| `examples/` | example harnesses (tested to stay valid) |
| `docs/` | vision, architecture, threat model, ADRs |
