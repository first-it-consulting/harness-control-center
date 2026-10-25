# ADR-0002: Out-of-process plugins over gRPC

- Status: accepted
- Date: 2026-10-25

## Context

Sandboxes, agent runtimes, transports, credential providers, protocols
(MCP, A2A) and notifiers must be pluggable. Some plugins need host access
(sandbox providers). Community plugins must not be able to compromise the core.

## Decision

- Use [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin): each
  plugin is a separate process, speaking a versioned gRPC interface over a
  local socket. A crashing or malicious plugin cannot read the core's
  memory or crash it.
- Each plugin ships a **manifest** declaring the capabilities it needs
  (filesystem paths, network, privileged helper calls). The user approves it
  in plain language at install time, and the core enforces it.
- Official plugins are signed in the same way as releases. Unsigned plugins
  require an explicit "I trust this" step.
- Interfaces live in `pkg/plugin/...` as protobuf, versioned
  independently of the core.

## Alternatives considered

- **Go `plugin` package**: in-process, fragile (exact toolchain match), no isolation. Rejected.
- **WASM (wazero)**: strong capability-based sandboxing, but cannot drive
  host virtualization. Kept as a future option for untrusted, pure-logic
  plugins (for example policy extensions).

## Consequences

Plugins are easy to write in any gRPC language. The cost is
serialization overhead (negligible at our call rates) and the need to
manage plugin process lifecycles.
