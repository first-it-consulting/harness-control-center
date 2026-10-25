# ADR-0004: Sandbox network access only through a per-VM egress proxy

- Status: accepted
- Date: 2026-10-25

## Context

Most useful agents need the network (at least their LLM API), yet network
access is the main way a prompt-injected agent exfiltrates data or attacks
the user's network. Harnesses grant egress as host names and ports
(`spec.permissions.network.egress`); that grant must be enforced outside
the agent's reach, by name, without trusting the guest's DNS or routing.

## Decision

- **No route out.** A networked VM gets one virtio-net interface on its own
  /30 (`hcc<slot>`, from a reserved subnet, default 172.31.0.0/16). The
  guest gets an address but **no gateway and no DNS**: the only reachable
  address is the host end of its link.
- **One proxy per VM**, inside the unprivileged runner, listening on that
  host address. Being per VM, it knows which policy applies without any
  authentication. It speaks HTTP CONNECT only; the guest's init sets
  `HTTPS_PROXY`.
- **The proxy decides** with `policy.CheckEgress` (names and ports only,
  IP literals refused), then resolves the name itself and connects to the
  first resolved address that is not loopback, link-local (cloud
  metadata), multicast, reserved, the sandbox subnet, or private (unless
  the harness grants `allowPrivateNetworks`). It dials exactly the address
  it checked, which defeats DNS rebinding. The host's own addresses are
  never destinations. Each proxy accepts connections only from its link's
  guest address. Every decision is logged.
- **The host enforces it too.** nftables table `inet hcc_guard` (applied
  atomically by `hcc-helper net-up`): from `hcc*` interfaces only new TCP
  connections to the proxy port *on the link's own address* are accepted;
  nothing is forwarded to or from sandbox links, and nothing arriving on
  another interface may reach the sandbox subnet.
- **Root work stays in hcc-helper** (`net-up`, `net-down`): tap devices
  owned by the slot's VM UID, addresses and the ruleset all derive from the
  root-owned configuration and the slot number.

## Consequences

- Plain HTTP (non-CONNECT) proxying is refused; HTTPS covers package
  registries, Git hosting and LLM APIs. Tools that ignore `HTTPS_PROXY`
  get no network.
- Interface names `hcc*` and the configured subnet are reserved on runner
  hosts.
- The credential broker (injecting secrets at the proxy) builds on this:
  it needs the proxy to terminate TLS for selected hosts, a later decision.
