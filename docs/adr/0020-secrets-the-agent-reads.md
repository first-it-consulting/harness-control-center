# ADR-0020: Secrets the agent itself reads (passwords, kubeconfigs)

- Status: accepted
- Date: 2026-11-17
- Amends: the "no secrets in sandboxes" rule (CONTRIBUTING)

## Context

People want to give agents a database password, a kubeconfig, an SSH key
or similar. Unlike an API key, these cannot be added to HTTP requests by
the egress proxy: the agent's own program (psql, kubectl, git over ssh)
needs the secret itself. So far every secret was kept out of the VM
(ADR-0005, ADR-0017, ADR-0019), and such harnesses were impossible.

## Options considered

1. **Inject more protocols in the proxy** (Kubernetes API, Git over HTTPS,
   database relays). The agent never holds a secret, but every protocol is
   work, and it is not flexible: whatever is not covered stays impossible.
2. **Short-lived, narrow credentials per job** (a GitHub App token for one
   repository, a Kubernetes token for one namespace, a Vault database
   user). The best limit on a leak, but each service needs its own minting
   and setup, which is too much for this project's newcomers.
3. **Hand a secret to the agent, explicitly**, as CI systems (GitHub
   Actions, GitLab CI) do: a job gets the secrets it is granted from the
   store when it starts.
4. **Not supporting it.** It would make the system useless for much real
   work.

## Decision

Option 3.

- **A credential in the store has a kind.** The default kind is added to
  web requests by the broker, and the agent never sees it. The kind
  `secret` is *for the agent*: any text up to 64 KiB, one line or a
  file. It has no hosts, and the broker never sends it.
- **A harness hands a secret to its agent explicitly** in
  `spec.permissions.secrets`, each as an environment variable (`env`) or a
  file below the agent's home (`file`). Deny by default. Variables the
  sandbox or runtime use (PATH, proxies, LD_*, ANTHROPIC_*, …) and files
  read as instructions (.bashrc, .profile, .claude/…) are refused. Paths
  are plain and relative.
- **Only a `secret` is ever handed over.** A credential for web requests
  is never given to an agent, whatever a harness asks, and a `secret` is
  never used for web requests. Changing a credential's kind needs a new
  value, so one kind cannot quietly become the other.
- **Risk is always high**, and `hcc explain` names each secret, where the
  agent gets it, and that it could send it to any host it may reach (or
  that it has no network).
- **Delivery**: the Control Center copies the secret to the runner with the
  other credentials before the job starts (ADR-0019). The runner reads it
  for that job and puts it into that job's task only, never into the
  job's record. The guest's init exports it or writes the file (0600) and
  tells the agent where it is, never what it is. No value is evaluated by
  a shell.
- **The console masks secrets** (`***`), line by line, and the token-like
  words of a secret file too, like CI systems do. This is a safety net,
  not a guarantee: an agent that changes a secret before printing it is
  not caught.
- **Helper agents never get secrets**, even if they ask.

## Consequences

- Harnesses can now work with databases, Kubernetes, SSH and anything
  else that needs a secret. The price is that the agent holds the secret:
  a misled agent can send it to any host its harness may reach. The
  network allowlist is what limits that, together with giving the agent
  only secrets that can do little.
- The secret is on the runner's disk while the job's task drive exists (in
  the runner's own staging directory, removed with the job), and in the
  runner store, as other credentials are.
- The sandbox still reaches the network only through the egress proxy.
  Programs without proxy support (psql, ssh) reach the allowed hosts
  through the guest's forwarder (ADR-0021).
