# ADR-0005: Credentials are added by the egress proxy, never given to the agent

- Status: accepted
- Date: 2026-10-25

## Context

Agents need credentials (LLM API keys, Git tokens). Whatever the agent
holds, a prompt injection can exfiltrate. The secret must therefore stay
outside the sandbox and be usable only towards the hosts it is meant for.

## Decision

- **Store.** `/etc/hcc/credentials.yaml` names each credential, the exact
  hosts it may be sent to, the header it goes in and a secret file.
  Store and secrets are root-owned and readable only by the runner's
  group; the daemon refuses a store others can read or the runner can
  change. Secrets with control characters (header injection) are
  rejected. The store is re-read for every job, so rotation needs no
  restart.
- **Grant.** A harness lists credential names. Each must exist, and each
  of its hosts must be in the harness's egress allowlist on port 443;
  otherwise the harness is rejected.
- **Injection.** For those hosts (port 443 only), the job's egress proxy
  terminates TLS, removes any header of that name the guest sent (and
  `Proxy-Authorization`), sets the secret, and forwards to the address it
  checked, verifying the upstream against the system roots. All other
  hosts remain end-to-end TLS tunnels the proxy cannot read.
- **Per-job CA.** An ECDSA CA is generated in the runner's memory for each
  job and discarded with it. Critical X.509 name constraints permit only
  the job's credential hosts and exclude all IP addresses, so the guest's
  trust in it extends to nothing else. The guest's init fetches its
  public certificate from the proxy and sets `SSL_CERT_FILE`,
  `NODE_EXTRA_CA_CERTS` and friends.
- **Plain HTTP only by explicit operator choice.** A credential may set
  `scheme: http` and a port (e.g. a local inference server). The proxy
  then forwards absolute-form HTTP requests for exactly that host and port,
  one host per connection, with the header replaced; every other plain
  HTTP request is still refused. The secret then crosses the network
  unencrypted, and harnesses using such an endpoint are rated higher risk
  and say "UNENCRYPTED" in `hcc explain`.
- **No secret in logs, errors or `%v` output.**

## Consequences

- A credential host that echoes request headers (e.g. httpbin.org) would
  show the secret to the agent. Credential hosts must be real APIs; the
  operator chooses them.
- An agent can still *use* a credential towards its hosts within the
  harness's limits (budget, approvals); the broker prevents theft, not use.
- HTTP/2 to brokered hosts is downgraded to HTTP/1.1. Tools that pin
  certificates or ignore the CA variables cannot use brokered hosts.
