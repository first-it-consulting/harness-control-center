# Vision

## The problem

Running an AI agent unattended, on a schedule or in a loop, with access to
real systems is powerful and dangerous. Today it means hand-built scripts,
containers, cron jobs and long-lived tokens on a server, and hoping the
agent does not misbehave. Only experts can do it, and even they often do
it unsafely.

## The product

A free, open-source **Control Center** where anyone can:

1. describe a task in plain words,
2. get a **harness** for it with safe defaults: prompt, schedule,
   sandbox, permissions, budget and approvals,
3. read in plain language what the agent will be able to do, with a risk rating,
4. run it on their own computer or their own servers (installed over SSH),
5. watch every agent: its state, logs, tool calls and spending. Stop any of them at any time.

## Principles

- **Safe by default, safe by design.** Least privilege everywhere. The safe
  path is the easy path. Limits cannot be switched off.
- **Newcomer-first.** No knowledge of containers, VMs, SSH or YAML needed.
  Experts get the full spec and plugins.
- **Your machines, your choice.** Local or self-hosted, any transport
  (SSH, Tailscale, WireGuard ...), any agent runtime. No lock-in.
- **Observable.** Every action of every agent can be traced and explained.
- **Extensible.** Sandboxes, runtimes, transports, credentials and
  protocols (MCP, A2A) are plugins.

## Not goals (for now)

- A hosted SaaS. Users run everything themselves.
- Multi-tenant team features (RBAC, SSO). The design must not prevent them later.
