# Threat Model

Living document. Update it in the same PR as any change that affects it.

## Assets

1. The user's **credentials** (Git hosting tokens, LLM API keys, SSH keys)
2. The user's **machines and private network** (laptop, servers, homelab)
3. The user's **money** (LLM spend, compute)
4. The **integrity** of the user's repositories and systems
5. The **trustworthiness of our releases**

## Adversaries

| Adversary | Assumption |
|---|---|
| **The agent itself** | The primary threat. It may be confused, wrong, or fully controlled through **prompt injection** by any content it reads (web pages, issues, READMEs, package metadata). We assume the agent is hostile and design the system to contain it. |
| Malicious or buggy plugin | Runs out of process with declared permissions; treated as untrusted ([ADR-0002](adr/0002-plugin-system.md)). |
| Network attacker | Between the Control Center and runners, and between the sandbox and the internet. |
| Supply-chain attacker | Targets our dependencies, CI or release artifacts. |
| Other local users on a runner host | Must not reach other users' agents, secrets or logs. |

## Controls

| Threat | Control | Status |
|---|---|---|
| Agent reads secrets | Secrets never enter the sandbox. The broker adds them at the egress proxy, per allowed host. | planned |
| Agent exfiltrates data | Default: no network at all. A networked VM has no gateway and no DNS; its only peer is a per-VM CONNECT proxy that allows only allowlisted host names and ports (IP literals refused), resolves names itself and dials only an address that is not loopback, link-local (cloud metadata), reserved, the sandbox subnet or private (unless `allowPrivateNetworks`), so DNS rebinding fails. nftables on the host additionally drops everything from sandbox links except the proxy port on their own address, and all forwarding. See ADR-0004. | done (e2e-tested) |
| Parser differential in egress checks (`evil.com#.allowed.org` suffix-matching a wildcard while the proxy connects elsewhere) | The policy engine matches only plain, label-validated DNS names. The proxy must pass the policy the same host string it then dials. | policy done, proxy planned |
| Malformed tool references reaching an installer (`pkg@1;rm -rf /`) | Requests must match the strict package syntax before any grant is checked. Installers must never use a shell. | policy done, installer planned |
| Host files copied into a guest (a crafted kernel or rootfs path, or a symlink swapped after the check) | The Firecracker provider only accepts images that resolve inside its configured `ImageRoot`, and copies from the already-opened, verified file handle. Harnesses name images; they never supply paths. | done |
| Harness runs with permissions the runner cannot enforce, or on a machine it was not approved for | The runner rejects harnesses that need network, credentials, tool installs, subagents, approvals or schedules until each enforcement point exists (fail closed), and rejects harnesses whose `spec.target` is not its own. | done |
| Guest reaches host services through the proxy | Every address configured on the host (read per connection; refusal if unreadable) is never a valid destination, even with `allowPrivateNetworks`. | done |
| Another machine uses a sandbox's proxy | nftables drops anything for the sandbox subnet not arriving on a sandbox link; each proxy accepts connections only from its own link's guest address. | done |
| Agent reaches private network | Internal names rejected by default; the proxy refuses private addresses unless `allowPrivateNetworks` (an explicit opt-in rated high risk). Loopback, link-local and the sandbox subnet are refused even then. | done |
| Sandbox escape | One microVM per run (separate kernel) under Firecracker's jailer: private mount namespace, seccomp, cgroup v2 limits, dropped UID. Read-only root filesystem, no network devices. Kernel arguments that change boot or init are rejected. | firecracker provider done (e2e-tested); private network/PID namespace for the VMM planned |
| Runner compromise escalates | The runner runs as the system user `hcc-runner`, whose only root access is one sudoers rule for `hcc-helper`. The helper accepts calls only from that user. It takes nothing but a job ID, an identity slot and the guest size (all bounds-checked), builds the jailer command from its root-owned config, and builds every jail itself in a tree that is root-owned and root-only-writable all the way up, so the runner cannot swap parts of a jail between check and use (no TOCTOU). The runner only stages files in its own directory; the helper reads them without following any symlink and copies only regular, singly linked files owned by the runner, so root never copies something the runner could not read itself. Tested on hcc-dev-1 by attacking the helper as hcc-runner. | done |
| Runaway cost or time | Timeout and budget are mandatory, bounded and covered by fuzz tests. The budget covers the whole subagent tree. Kill switch: SIGTERM, then `cgroup.kill`; a VM that cannot be stopped is reported as an error. | spec done, sandbox timeout + kill switch done (e2e-tested), budget enforcement planned |
| Privilege amplification through subagents | A child receives only a subset of the parent's permissions (`policy.CheckChild`). Spawn count is capped. | policy done |
| Destructive actions | Approval gates pause the agent until a human decides. | policy done, runtime planned |
| User misunderstands permissions | Plain-language `explain` and a risk rating before every start. The spec is strict (unknown fields are errors, single document, size-limited). | done |
| Malicious plugin | Out-of-process plugins with a permission manifest approved by the user. Official plugins are signed. | planned |
| Control-plane MITM | The runner listens on no network port. `hcc --ssh` reaches it through the user's own ssh (their host keys and pinning), relaying to the Unix socket. | done (ssh); Control Center TOFU pinning planned |
| Other local users drive the runner | The socket is 0600 in a 0700 directory, owned by the runner user; verified that another user gets "permission denied". | done |
| Option injection through the ssh host (`--ssh -oProxyCommand=…`) | Hosts must not start with '-', ssh gets "--" before the host, and the remote socket path is restricted to shell-inert characters. | done |
| Runner user becomes root by replacing the jailer | The daemon refuses to start unless the jailer, Firecracker and every parent directory are root-owned and not group/other-writable. Images and config are root-owned and read-only for the runner. The runner refuses to run as root. | done |
| Guest fills the host disk through its console | Console logs are capped at 10 MiB per job; further output is dropped. | done (no rotation of old jobs yet) |
| Tampered release | Keyless cosign signatures, SBOMs, SLSA provenance, SHA-pinned actions, least-privilege workflow tokens. | pipeline done |
| Log leakage | Secrets are redacted at the broker. Logs stay on the user's machines. | planned |

## Known limitations

- Wildcard egress (`*.example.com`) cannot tell registrable domains from
  public suffixes such as `*.co.uk`. Users are warned via the risk rating.
- An allowlisted host can itself be an exfiltration channel (for example,
  pushing to a public repository on an allowed Git host). Approval gates and
  narrowly scoped tokens reduce, but do not eliminate, this risk.
- The cron syntax check is shallow. Full parsing arrives with the scheduler.
