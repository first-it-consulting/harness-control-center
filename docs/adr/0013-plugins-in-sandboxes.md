# ADR-0013: Plugins run in sandboxes of their own

- Status: accepted (supersedes ADR-0002)
- Date: 2026-10-26

## Context

ADR-0002 planned host processes speaking gRPC (hashicorp/go-plugin), with
a manifest the core would enforce. A host process runs with the runner
user's rights, though: its manifest could only be enforced in part, and
the most dangerous thing a newcomer installs would be the least
isolated. We already have a boundary that enforces exactly a declared
set of permissions: the sandbox an agent runs in.

## Decision

- **A plugin is a directory**: `plugin.yaml` (`spec.Plugin`: name,
  version, type, command, sandbox size, and permissions in the
  harness's own terms, without subagents) and its files. Installing it
  (`hcc plugin install DIR`) re-packs it into a clean archive (plain files
  and directories, safe names; anything else refuses the install), shows
  in plain words what it may do with a risk rating, and installs exactly
  the archive whose SHA-256 the operator confirmed. Its checksum is
  checked again before every run. No upgrades in place: remove, then
  install.
- **Every run is a sandbox**: a Firecracker VM like a job's, built from
  a harness made of the manifest, so it gets that harness's egress proxy,
  credential broker and tools, and nothing else. Its files arrive on a
  read-only drive; its command runs as the unprivileged user in its
  directory. Plugin VMs have identities of their own (`maxPluginVMs`,
  default 2), so they never take a job's slot, and they are not jobs, so
  they cause no events of their own.
- **Any language, simple contracts.** A `notifier` gets the events it
  subscribed to (`job.ended`, `job.awaiting_approval`, `schedule.paused`)
  as JSON lines on stdin, in batches, and exits; failures are logged and
  not retried. An `mcp-server` is a stdio MCP server that agents use
  (ADR-0014).
- **Signing later.** There are no official plugins yet. Every plugin is
  shown as unsigned and needs an explicit confirmation of its checksum.

## Consequences

- Plugins cannot extend the host (sandbox providers, transports): those
  stay in the core until a need justifies a privileged plugin kind.
- A run costs a VM boot (about a second) plus its tools; notifiers are
  batched so this stays rare.
- A notifier learns about every job on its runner, including text agents
  wrote; the explanation says so and rates it accordingly.
