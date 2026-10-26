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
| Agent reads secrets | Secrets never enter the sandbox: the egress proxy adds them, only for their own hosts (which must be in the harness's allowlist), replacing any header of that name the guest sent. TLS for those hosts is terminated with a per-job, in-memory CA whose name constraints allow only those hosts. Secrets never appear in logs. See ADR-0005. | done (unit, real-internet and VM tests) |
| Agent exfiltrates a secret through an echoing host | Inherent to injection: hosts that echo request headers would reveal the secret. Credential hosts are chosen by the operator and must be real APIs. | accepted risk, documented |
| Agent exfiltrates data | Default: no network at all. A networked VM has no gateway and no DNS; its only peer is a per-VM CONNECT proxy that allows only allowlisted host names and ports (IP literals refused), resolves names itself and dials only an address that is not loopback, link-local (cloud metadata), reserved, the sandbox subnet or private (unless `allowPrivateNetworks`), so DNS rebinding fails. nftables on the host additionally drops everything from sandbox links except the proxy port on their own address, and all forwarding. See ADR-0004. | done (e2e-tested) |
| Parser differential in egress checks (`evil.com#.allowed.org` suffix-matching a wildcard while the proxy connects elsewhere) | The policy engine matches only plain, label-validated DNS names. The proxy must pass the policy the same host string it then dials. | policy done, proxy planned |
| Malformed tool references reaching an installer (`pkg@1;rm -rf /`) | Requests must match the strict package syntax and name a package the image's catalog offers; init installs only file names from the catalog list, checked again, passed as arguments (no shell parsing). | done (tested, e2e) |
| Agent installs or tampers with tools | Tools come from a root-owned catalog of Alpine-signed packages built at runner install; init installs them before the agent starts, offline, with signature checks, then unmounts the catalog (its device is root's). The agent is unprivileged and has no package repository. See ADR-0010. | done (tested for real on hcc-dev-1) |
| Host files copied into a guest (a crafted kernel or rootfs path, or a symlink swapped after the check) | The Firecracker provider only accepts images that resolve inside its configured `ImageRoot`, and copies from the already-opened, verified file handle. Harnesses name images; they never supply paths. | done |
| Harness runs with permissions the runner cannot enforce, or on a machine it was not approved for | The runner rejects harnesses that need what it cannot enforce (tools its image does not offer, subagents, approval actions outside the enforceable set, network without an egress proxy), and harnesses whose `spec.target` is not its own. | done |
| Guest reaches host services through the proxy | Every address configured on the host (read per connection; refusal if unreadable) is never a valid destination, even with `allowPrivateNetworks`. | done |
| Another machine uses a sandbox's proxy | nftables drops anything for the sandbox subnet not arriving on a sandbox link; each proxy accepts connections only from its own link's guest address. | done |
| Agent reaches private network | Internal names rejected by default; the proxy refuses private addresses unless `allowPrivateNetworks` (an explicit opt-in rated high risk). Loopback, link-local and the sandbox subnet are refused even then. | done |
| Sandbox escape | One microVM per run (separate kernel) under Firecracker's jailer: private mount namespace, seccomp, cgroup v2 limits, dropped UID. Read-only root filesystem, no network devices. Kernel arguments that change boot or init are rejected. | firecracker provider done (e2e-tested); private network/PID namespace for the VMM planned |
| Runner compromise escalates | The runner runs as the system user `hcc-runner`, whose only root access is one sudoers rule for `hcc-helper`. The helper accepts calls only from that user. It takes nothing but a job ID, an identity slot and the guest size (all bounds-checked), builds the jailer command from its root-owned config, and builds every jail itself in a tree that is root-owned and root-only-writable all the way up, so the runner cannot swap parts of a jail between check and use (no TOCTOU). The runner only stages files in its own directory; the helper reads them without following any symlink and copies only regular, singly linked files owned by the runner, so root never copies something the runner could not read itself. Tested on hcc-dev-1 by attacking the helper as hcc-runner. | done |
| Runaway cost or time | Timeout and budget are mandatory and bounded. The model endpoint is always brokered, and the proxy reads token usage from its responses (Anthropic and OpenAI formats, JSON and streaming, recorded as it streams); once `budget.maxTokens` (or `maxCostUSD`, for models the runner has a price for) is reached, further model requests get 429 and the job ends `over_budget`. Metering fails closed: responses are requested uncompressed and a compressed one is refused; every POST is metered unless known to be free; non-canonical paths are refused; OpenAI streams are asked to report usage; a response that ends without its final usage is charged its `max_tokens`, or stops the budget if it has none. At most 4 model requests run at once, so a limit can be overshot by at most those. Kill switch: SIGTERM, then `cgroup.kill`. | done (e2e-tested on hcc-dev-1) |
| A scheduled or looping agent repeats a mistake unattended | Schedules live on the runner and start ordinary jobs with the harness's limits; at most one run per `minScheduleInterval` (default 15 min); no overlapping runs; no catch-up after downtime; loops stop at the first unsuccessful run; the kill switch pauses every schedule and resuming the runner does not unpause them; the explanation, risk rating and confirmation say it runs unattended. See ADR-0008. | done (tested, real cron and loop runs on hcc-dev-1) |
| Agent fakes "task complete" to end, or keep, a loop | Completion counts only as init's marker right before its result line, printed after the agent exited. | done (tested) |
| Privilege amplification through subagents | A child receives only a subset of the parent's permissions (`policy.CheckChild`). Spawn count is capped. | policy done |
| Destructive actions | Approval gates hold matching HTTP requests (e.g. `git.push`, `http.delete`) at the egress proxy, before they leave, until a human approves; with gates every allowed host is inspected, so nothing bypasses them; unenforceable actions are rejected; unanswered requests are refused. See ADR-0009. | done (tested; approved and denied for real on hcc-dev-1) |
| The agent hides what it asks to do (escape sequences in a path, more ref updates than shown) | Request details are shown as inert text in CLI and UI; a push whose ref updates cannot all be read is refused, never offered. | done |
| User misunderstands permissions | Plain-language `explain` and a risk rating before every start. The spec is strict (unknown fields are errors, single document, size-limited). | done |
| Malicious plugin | Out-of-process plugins with a permission manifest approved by the user. Official plugins are signed. | planned |
| A web page or local process drives the Control Center (CSRF, DNS rebinding, a session cookie collected by another local port) | Loopback only; a session exists only through the secret login link printed at start, and is no cookie: it travels in a header from the page's sessionStorage (port-scoped), handed over in the URL fragment; the Host header must be the server's; changes need a same-origin Origin and a JSON body; downloads use single-use, one-minute tickets. See ADR-0007. | done (tested) |
| Agent output attacks the user through the UI (HTML or script injection, bidi tricks) | The UI builds the DOM only with textContent (a test forbids HTML parsing), the CSP allows no inline or foreign script, the console is neutralized server-side, results are downloads only. | done (tested) |
| User starts a harness without understanding it | The UI shows the explanation and risk of exactly that document; starting needs an explicit confirmation, which the API also requires. | done |
| Control-plane MITM | The runner listens on no network port. `hcc --ssh` reaches it through the user's own ssh (their host keys and pinning), relaying to the Unix socket. | done (ssh); Control Center TOFU pinning planned |
| Other local users drive the runner | The socket is 0600 in a 0700 directory, owned by the runner user; verified that another user gets "permission denied". | done |
| Option injection through the ssh host (`--ssh -oProxyCommand=…`) | Hosts must not start with '-', ssh gets "--" before the host, and the remote socket path is restricted to shell-inert characters. | done |
| Runner user becomes root by replacing the jailer | The daemon refuses to start unless the jailer, Firecracker and every parent directory are root-owned and not group/other-writable. Images and config are root-owned and read-only for the runner. The runner refuses to run as root. | done |
| Guest fills the host disk through its console | Console logs are capped at 10 MiB per job; further output is dropped. | done |
| Guest-written results attack the host or the user (symlinks, `../` paths, device files, setuid bits, terminal escapes in names, a disk swapped for a link) | Root never mounts or parses the guest's disk: the helper only streams the bytes of a regular, singly linked file opened without following links. The runner re-packs the tar unprivileged, keeping only regular files and directories with clean relative names and normalized metadata, within size and count limits (fuzzed). `hcc results` never overwrites and never writes to a terminal. See ADR-0006. | done (e2e-tested) |
| Runner crash leaves VMs running or holding resources | Jobs are persisted on every state change. On start, unfinished jobs are marked failed and their sandboxes cleaned up before any new job runs: cgroup killed, jail and staging removed, every sandbox link deleted (verified on hcc-dev-1 by SIGKILLing the runner mid-job). | done |
| Guest fills the host disk over time | Finished jobs (record, console, results) are pruned by count and age (`retention`, defaults 1000 jobs, 30 days). | done |
| Guest fills the host disk through its results disk | The disk is sparse but its full size is reserved against the jail filesystem's free space (including what other jails' disks may still grow); stored results are bounded by `diskMiB` and need free space on the state directory. | done |
| Tampered release | Keyless cosign signatures, SBOMs, SLSA provenance, SHA-pinned actions, least-privilege workflow tokens. | pipeline done |
| Log leakage | Credentials format without their secret (`%v`, `%#v`); the broker logs credential names, hosts, paths and status codes only (tested). Logs stay on the user's machines. | done |

## Known limitations

- Wildcard egress (`*.example.com`) cannot tell registrable domains from
  public suffixes such as `*.co.uk`. Users are warned via the risk rating.
- An allowlisted host can itself be an exfiltration channel (for example,
  pushing to a public repository on an allowed Git host). Approval gates and
  narrowly scoped tokens reduce, but do not eliminate, this risk.
