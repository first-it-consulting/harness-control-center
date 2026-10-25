# ADR-0003: Control Center connects out to runners; SSH first, transport pluggable

- Status: accepted
- Date: 2026-10-25

## Context

Laptops sit behind NAT and sleep. Servers usually cannot reach the user's
laptop, and should not need to. Users already have SSH access to their servers.
Some users prefer Tailscale, WireGuard or other networks.

## Decision

- The **Control Center always initiates** connections to runners. Runners
  expose nothing to the network. They listen only on a local Unix socket.
- The first transport is **SSH**. It is used both to install or upgrade
  the runner and to tunnel the gRPC control API to the runner's socket. Host
  keys are pinned on first use, and changes are hard errors.
- Transports are a plugin type (`Transport`), so Tailscale, WireGuard or mTLS
  over direct TCP can be added without core changes. The user picks per server.
- Runners are autonomous: schedules, budgets and the kill switch work
  without a connection. Events are buffered and replayed on reconnect.

## Consequences

No inbound ports on servers and no relay service. "Stop everything now"
requires reaching the runner, so each runner also enforces its own limits
locally, and a runner that has lost contact for longer than a configurable
period can pause its scheduled harnesses.
