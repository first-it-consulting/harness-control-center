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
| Agent exfiltrates data | Default: no network. Egress only to allowlisted host names and ports. IP literals rejected. The resolved IP is re-checked at the proxy (blocks DNS rebinding, private ranges, cloud metadata `169.254.169.254`) unless `allowPrivateNetworks` is set. | spec + policy done, proxy planned |
| Agent reaches private network | Internal names rejected by default. `allowPrivateNetworks` is an explicit opt-in rated high risk. | spec + policy done |
| Sandbox escape | One microVM per run (separate kernel) under Firecracker's jailer: chroot, seccomp, cgroups, dropped UID. | planned |
| Runner compromise escalates | The runner is unprivileged. Only a minimal helper holds privileges. | planned |
| Runaway cost or time | Timeout and budget are mandatory, bounded and covered by fuzz tests. The budget covers the whole subagent tree. Kill switch. | spec done, enforcement planned |
| Privilege amplification through subagents | A child receives only a subset of the parent's permissions (`policy.CheckChild`). Spawn count is capped. | policy done |
| Destructive actions | Approval gates pause the agent until a human decides. | policy done, runtime planned |
| User misunderstands permissions | Plain-language `explain` and a risk rating before every start. The spec is strict (unknown fields are errors, single document, size-limited). | done |
| Malicious plugin | Out-of-process plugins with a permission manifest approved by the user. Official plugins are signed. | planned |
| Control-plane MITM | SSH with host-key pinning on first use (later mTLS). No plaintext transport. | planned |
| Tampered release | Keyless cosign signatures, SBOMs, SLSA provenance, SHA-pinned actions, least-privilege workflow tokens. | pipeline done |
| Log leakage | Secrets are redacted at the broker. Logs stay on the user's machines. | planned |

## Known limitations

- Wildcard egress (`*.example.com`) cannot tell registrable domains from
  public suffixes such as `*.co.uk`. Users are warned via the risk rating.
- An allowlisted host can itself be an exfiltration channel (for example,
  pushing to a public repository on an allowed Git host). Approval gates and
  narrowly scoped tokens reduce, but do not eliminate, this risk.
- The cron syntax check is shallow. Full parsing arrives with the scheduler.
