# Deploying a runner

A runner is `hcc runner serve` on a Linux host with KVM, managed by
systemd. It listens only on a Unix socket (`/run/hcc/runner.sock`, owner
only); you reach it over ssh with `hcc --ssh <host>`.

```sh
# 1. once, on the host: fetch and verify Firecracker, kernel and rootfs
ssh <host> 'bash -s' < hack/firecracker-poc/run.sh

# 2. from your machine: build, install and (re)start the runner
make deploy-runner HOST=<host> TARGET=server:<name>

# 3. run a harness whose spec.target is server:<name>
hcc run --ssh <host> my-harness.yaml
```

## What gets installed

| Path | Owner | Why |
|---|---|---|
| `/usr/local/bin/hcc`, `/usr/local/lib/hcc/{jailer,firecracker}` | root, 0755 | The jailer runs as root via sudo; whoever can modify it is root. The daemon refuses to start otherwise. |
| `/usr/local/lib/hcc/images/` | root, read-only | The runner boots images but cannot change them. |
| `/etc/hcc/runner.yaml` | root, 0644 | The runner cannot change its own configuration. |
| `/var/lib/hcc/` (state, jails), `/run/hcc/` (socket) | runner user, 0700 | Nobody else can reach jobs or logs. |
| `/etc/systemd/system/hcc-runner.service` | root | Hardened unit; see comments inside. |

## Current limitations

- The runner runs as the installing user, which needs non-interactive
  sudo. A dedicated system user with a sudoers rule limited to the jailer
  (and, later, a privileged helper instead of sudo) is planned.
- The `base` image boots and shuts down; no agent runtime runs in it yet.
- Job history lives in memory and is lost on restart; console logs stay in
  `/var/lib/hcc/state` and are not rotated yet.
