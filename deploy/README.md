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
| `/etc/hcc/credentials.yaml`, `/etc/hcc/secrets/` | root:hcc-runner, 0640 / 0750 | The credential broker's store: readable by the runner, by nobody else; the runner cannot change it. |
| `/etc/hcc/runner.yaml` | root, 0644 | Neither the runner nor the caller can change what runs as root. |
| `/var/lib/hcc-jails/` | root, 0711 | The jail tree. hcc-helper builds every jail itself from the runner's staged files; the runner cannot list, add or swap anything there. |
| `/var/lib/hcc/` (state, staging) | hcc-runner, 0700 | Only the runner. It prepares each VM's files in `staging/<job>`. |
| `inet hcc_guard` (nftables), `hcc*` interfaces, 172.31.0.0/16 | root | Sandbox links and their guard (ADR-0004). The names and the subnet are reserved; change `network.subnet` in `/etc/hcc/runner.yaml` if it overlaps a real network. |
| `/run/hcc/runner.sock` | hcc-runner:hcc, 0660 in a 0750 dir | The runner and group `hcc`; nobody else. |

## Adding a credential

Secrets live on the runner host and never enter a sandbox; the egress
proxy adds them to requests for their hosts (ADR-0005).

```sh
# on the runner host
sudo install -o root -g hcc-runner -m 0640 /dev/stdin /etc/hcc/secrets/anthropic <<<"sk-ant-..."
sudoedit /etc/hcc/credentials.yaml
```

```yaml
# /etc/hcc/credentials.yaml (replace the "{}" placeholder)
anthropic:
  hosts: [api.anthropic.com]
  header: x-api-key
  secretFile: /etc/hcc/secrets/anthropic
```

A harness then grants it with `permissions.credentials: [anthropic]` and
must allow `api.anthropic.com` in its egress. Changes apply to the next
job.

A local inference server that speaks plain HTTP (e.g. oMLX, Ollama) needs
`scheme: http` and its `port`; the key then crosses your network
unencrypted:

```yaml
omlx:
  hosts: [llm.fritz.box]
  scheme: http
  port: 4000
  header: x-api-key
  secretFile: /etc/hcc/secrets/omlx
```

## Budgets and prices

Every job's model traffic is metered at the egress proxy; `hcc jobs`
shows the tokens used. `budget.maxTokens` always applies. `budget.maxCostUSD`
applies only to models the runner has a price for (USD per million
tokens) in `/etc/hcc/runner.yaml`:

```yaml
pricing:
  claude-sonnet-5-5: {input: 3, output: 15}
```

A job that reaches its budget is stopped and ends `over_budget`. At most
4 model requests run at once per job, so a limit can be overshot by at
most those. Metering fails closed: a model response whose usage cannot be
read (compressed, cut short without `max_tokens`, or missing) also stops
the job.

## Results

Every job gets a blank disk of `spec.sandbox.diskMiB` (default 4096,
at most `maxDiskMiB` in `/etc/hcc/runner.yaml`, default 8192). It is
sparse, but its full size is reserved on the jail filesystem while the
job runs. The `claude-code` image archives the agent's workspace onto it;
after a clean shutdown the runner re-packs that archive (plain files and
directories only) into `/var/lib/hcc/state/jobs/<id>/results.tar`.

```sh
hcc results <job> -o results.tar --ssh <host>
```

See ADR-0006 for what is kept and why.

## Schedules

A harness with `spec.trigger` becomes a schedule when you start it
(`hcc run`, or the web UI). The runner keeps it in
`/var/lib/hcc/state/schedules/` and starts its runs itself:

```sh
hcc schedules --ssh <host>
hcc schedule pause|resume|delete <schedule> --ssh <host>
```

Cron times are UTC. A schedule may not run more often than
`minScheduleInterval` in `/etc/hcc/runner.yaml` (default `15m`). Runs never
overlap, missed runs are not caught up, and the kill switch pauses every
schedule (resume each one afterwards). See ADR-0008.

## Approvals

Harnesses with `spec.approvals.requireFor` hold matching requests at the
egress proxy until someone decides:

```sh
hcc approvals --ssh <host>
hcc approve|deny <approval> --ssh <host>
```

The web UI shows them on every page. Unanswered requests are refused
after `approvalTimeout` in `/etc/hcc/runner.yaml` (default `30m`). See
ADR-0009.

## Tools

`install-runner.sh` gives the `claude-code` image a catalog of tools that
harnesses may ask for in `spec.permissions.tools.install`. Choose them with
`HCC_TOOLS` (Alpine package names; `""` for none) when installing:

```sh
HCC_TOOLS="python3 py3-pip nodejs npm make gcc musl-dev curl go" ./install-runner.sh ...
```

The runner rejects harnesses asking for anything else. Packages and their
dependencies are fetched once, as signed Alpine packages, test-installed
offline, and installed into each VM before its agent starts. The Control
Center's Runners page lists what each image offers. See ADR-0010.

## Subagents

A harness with `spec.permissions.subagents.max` may start helper agents,
each an ordinary job in its own VM with at most its parent's permissions
and the parent's budget and approval gates (ADR-0011). Inside the
`claude-code` image the agent uses `hcc-subagent`:

```sh
echo "write the tests" | hcc-subagent start --egress registry.npmjs.org --timeout 10m
hcc-subagent wait <id>; hcc-subagent results <id> out.tar
```

Helpers need a free slot (`maxConcurrent`), queue ahead of other jobs and
stop when their parent ends. `hcc jobs` shows who started each job.

## Job history

Each job is kept in `/var/lib/hcc/state/jobs/<id>/` (owner-only): its
record `job.json` (saved on every state change), console log and
results. History survives restarts. A job that was not finished when
the runner stopped is marked `failed` on the next start, and whatever its
sandbox left behind is cleaned up first: a crashed runner can leave VMs
running (the jailer moves them out of the service's cgroup), so the
runner kills their cgroups, removes their jails and staging, and deletes
the sandbox links.

Finished jobs are pruned hourly, by count and by age:

```yaml
retention:
  maxJobs: 1000   # default
  maxAge: 720h    # default (30 days)
```

The daemon's own log goes to journald, which rotates it.

## Images

| Image | Contents | Use |
|---|---|---|
| `base` | Alpine minirootfs | tests (boot, stop, network guard) |
| `claude-code` | Alpine, Claude Code (native musl build, pinned version and sha512), bash, git, jq, ripgrep; a tool catalog | `runtime: claude-code` harnesses with `sandbox.image: claude-code` |

Claude Code runs as the unprivileged user `agent` with its home and
workspace in memory, and prints its result between `HCC-RESULT-BEGIN` and
`HCC-RESULT-END exit=<status>` (a non-zero status fails the job). Its
workspace is handed back as the job's results.

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
- Only HTTPS (CONNECT) leaves a sandbox.
- Credential hosts that echo request headers would show the secret to the
  agent; only configure real APIs.
- A job's token usage is saved with its state changes, so a job that was
  running when the runner crashed shows the usage it had when it
  started.
