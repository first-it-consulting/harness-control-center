# ADR-0021: Programs without proxy support reach the allowed hosts

- Status: accepted
- Date: 2026-11-17

## Context

A sandbox's only way out is its job's egress proxy (ADR-0004): CONNECT
host:port, allowed by name and again by resolved address. Programs that
honour HTTPS_PROXY (curl, git over HTTPS, kubectl, most API clients) use
it. Others (psql and other database clients, ssh and git over ssh)
connect directly, find no route and no DNS, and fail, even when the
harness allows their host and hands the agent their password (ADR-0020).

## Decision

The guest maps the allowed hosts to itself and tunnels them through the
proxy:

- The runner lists the **exact hosts** of the harness's egress allowlist
  with their ports in the task (`forward`), each with a loopback address
  of its own (`127.77.0.N`). Wildcard rules cannot be listed; only
  proxy-aware programs reach those.
- The guest's init adds them to `/etc/hosts` (a copy in memory, bound over
  the read-only original) and starts `hcc-guest tcp-forward` as `nobody`.
  It listens on each address and allowed port, and carries every
  connection to the proxy as `CONNECT host:port`. To listen on ports
  below 1024 (ssh's 22) as `nobody`, init lowers the VM's
  `ip_unprivileged_port_start`; without that setting it runs as root.
- **The proxy stays the only enforcement point.** Nothing new is allowed:
  the forwarder only offers what the allowlist already allows, and the
  proxy checks each tunnel by name and resolved address as before. No
  netfilter rules or DNS server are needed in the guest.
- Names, addresses and ports from the task are checked again in init and
  by the forwarder.

## Consequences

- psql, mysql, redis-cli, ssh, git over ssh and database drivers work
  with hosts the harness allows (the tools themselves must be in the
  image's tool list).
- With approval gates, the proxy inspects every connection as TLS and
  refuses anything else (fail closed, ADR-0009). Such harnesses still
  reach only HTTPS.
- Programs that try to resolve a name that is not allowed get no answer,
  as before.
