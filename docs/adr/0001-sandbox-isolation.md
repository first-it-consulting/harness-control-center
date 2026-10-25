# ADR-0001: Every agent runs behind a VM boundary

- Status: accepted
- Date: 2026-10-25

## Context

We assume agents can be hostile (prompt injection). Containers share the
host kernel, and a single kernel bug is a full escape. Our audience cannot be
expected to harden container setups themselves.

## Decision

Every agent run gets its own kernel:

| Runner host | Sandbox |
|---|---|
| Linux with KVM (servers, Proxmox VMs with nested virtualization) | **Firecracker** microVM under the `jailer` (default) |
| macOS / Windows "local" | The runner runs inside a lightweight Linux VM that the Control Center manages (Apple Virtualization framework or Lima on macOS, WSL2 or Hyper-V on Windows). Inside it: Firecracker where nested virtualization works, otherwise gVisor. The VM is the boundary. |
| Proxmox, as an alternative | `proxmox` sandbox plugin: one VM per run, cloned from a template through the Proxmox API |

Plain Docker without gVisor is never a default. It may exist as an
explicitly unsafe developer plugin.

## Consequences

- Strong isolation and fast starts (Firecracker boots in about 125 ms).
- Root is needed for the jailer (and, later, tap devices and firewall
  rules), so a separate minimal **privileged helper** (`hcc-helper`) does
  that work. The runner stays unprivileged and its only sudo right is the
  helper. Implemented for start, kill and remove; networking follows.
- We must build and ship microVM kernels and root filesystems (images),
  signed like the binaries.
- End-to-end tests need KVM in CI (GitHub's Linux runners provide
  `/dev/kvm`; to be confirmed when the e2e suite lands).
