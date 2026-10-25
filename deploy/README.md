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

| Path / account | Owner | Why |
|---|---|---|
| user `hcc-runner` | system user, no login, no password | Runs the daemon. Its only root access is one sudoers rule for `hcc-helper`. |
| group `hcc` | | May use the runner socket. The installing admin user is added; `hcc --ssh` works through it. |
| `/etc/sudoers.d/hcc-runner` | root, 0440 | `hcc-runner ALL=(root) NOPASSWD: /usr/local/lib/hcc/hcc-helper`, checked with `visudo -c` before install. |
| `/usr/local/lib/hcc/hcc-helper` | root, 0755 | The only code that runs as root: start, kill, remove a jail. It builds the jailer command from `/etc/hcc/runner.yaml` and re-validates every argument. |
| `/usr/local/bin/hcc`, `/usr/local/lib/hcc/{jailer,firecracker}` | root, 0755 | Whoever can modify these is root. The daemon and helper refuse to start otherwise. |
| `/usr/local/lib/hcc/images/` | root, read-only | The runner boots images but cannot change them. |
| `/etc/hcc/runner.yaml` | root, 0644 | Neither the runner nor the caller can change what runs as root. |
| `/var/lib/hcc-jails/` | root, 0711 | The jail tree. hcc-helper builds every jail itself from the runner's staged files; the runner cannot list, add or swap anything there. |
| `/var/lib/hcc/` (state, staging) | hcc-runner, 0700 | Only the runner. It prepares each VM's files in `staging/<job>`. |
| `inet hcc_guard` (nftables), `hcc*` interfaces, 172.31.0.0/16 | root | Sandbox links and their guard (ADR-0004). The names and the subnet are reserved; change `network.subnet` in `/etc/hcc/runner.yaml` if it overlaps a real network. |
| `/run/hcc/runner.sock` | hcc-runner:hcc, 0660 in a 0750 dir | The runner and group `hcc`; nobody else. |

## Testing an installation

```sh
make e2e E2E_HOST=<host>   # real VMs, as hcc-runner, through hcc-helper
```

It stops the daemon while it runs (they would share VM identities) and
starts it again afterwards.

## Current limitations

- The systemd unit cannot use `NoNewPrivileges` or `ProtectSystem=strict`,
  because sudo needs setuid. Replacing sudo with a socket-activated root
  service would allow that.
- The `base` image boots and shuts down; no agent runtime runs in it yet.
  It does set up its network (eth0, `HTTPS_PROXY`) when the harness grants
  egress.
- Only HTTPS (CONNECT) leaves a sandbox; credentials cannot be injected
  yet, so harnesses that need them are still rejected.
- Job history lives in memory and is lost on restart; console logs stay in
  `/var/lib/hcc/state` and are not rotated yet.
