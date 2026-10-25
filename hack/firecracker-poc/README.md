# Firecracker proof of concept

`run.sh` boots a minimal, offline microVM under the Firecracker `jailer` and
reports how it is confined. It validates [ADR-0001](../../docs/adr/0001-sandbox-isolation.md)
before the `firecracker` sandbox provider is written.

```sh
# on a Linux host with /dev/kvm, as a normal user with sudo
./run.sh
```

## What it does

1. Downloads Firecracker, a guest kernel and an Alpine minirootfs. Each download is
   pinned to a version and checked against a SHA-256. A mismatch aborts the run.
2. Builds a read-only ext4 root filesystem **without root** (`mkfs.ext4 -d`),
   with an `/init` that reports and powers off.
3. Runs only the jailer as root. It applies cgroup v2 limits, `pivot_root`s
   into a private mount namespace, drops to UID/GID 64000 (no account),
   and execs Firecracker, which installs its seccomp filters.
4. Inspects the running VMM from the host, then removes the chroot and cgroup.

## Results (2026-10-25, `hcc-dev-1`: nested KVM on Proxmox, i5-8500T)

| Check | Result |
|---|---|
| Guest userspace reached | ~850 ms from jailer start (3 runs: 847-889 ms) |
| VMM user | uid 64000 (not root) |
| seccomp | mode 2 (filter active) |
| Mount namespace | private; VMM sees only `config.json dev firecracker firecracker.pid rootfs.ext4 run vmlinux` |
| cgroup v2 | `/firecracker/<id>`, `memory.max=256MiB`, `pids.max=64` |
| Guest network | `lo` only (deny by default) |
| Guest root filesystem | read-only |
| Cleanup | no chroot or cgroup left behind |

Boot time is measured under **nested** virtualization, through an emulated
serial console. Expect much less on bare-metal KVM. `quiet` alone took it
from ~2.1 s to ~0.85 s.

## Follow-ups for the real provider

- **Kernel supply chain:** the Firecracker CI kernel has no published
  checksum. It is pinned on first use here. Releases must build and sign their own.
- **Networking:** tap device plus nftables, and only toward the egress proxy.
  That needs the privileged helper (ADR-0001).
- **Root filesystem ownership:** files keep the build user's UID. Use a
  build step with explicit ownership (e.g. `mkfs.ext4 -d` from a tar with
  numeric owners) for images that have non-root users.
- **No sudo in production:** the runner calls a minimal privileged helper
  instead of running the jailer through sudo.
