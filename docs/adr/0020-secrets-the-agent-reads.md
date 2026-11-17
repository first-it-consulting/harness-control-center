# ADR-0020: Secrets the agent itself reads (passwords, kubeconfigs)

- Status: proposed (needs the maintainer's decision; not implemented)
- Date: 2026-11-17

## Context

People want to give agents a database password, a kubeconfig, an SSH key
or similar. Unlike an API key, these cannot be added to HTTP requests by
the egress proxy: the agent's own program (psql, kubectl, git over ssh)
needs the secret itself. CONTRIBUTING says "No secrets in sandboxes"; the
whole design so far (ADR-0005, ADR-0017, ADR-0019) keeps every secret out
of the VM.

## Options

1. **Keep the rule.** Support only what the proxy can inject (HTTP headers),
   and add more injection: Kubernetes API (bearer token or client
   certificate, the proxy terminating TLS towards the API server), Git over
   HTTPS (Basic with a token), database protocols (a proxy per protocol,
   e.g. PostgreSQL's auth handled by a local relay). Safest, but each
   protocol is work, and some (client certificates) need the proxy to hold
   keys.
2. **Allow handing a secret to the agent, explicitly.** A credential of kind
   "file" or "environment variable", attached to a harness, is written into
   the sandbox at start. Deny by default; the risk rating becomes high
   ("the agent can read and leak a secret"), `hcc explain` says which
   secret and where, and the egress allowlist is the only thing that keeps
   it from leaving. Simple and general, but it breaks the rule, and a
   prompt-injected agent can send the secret to any allowed host.
3. **Both:** injection where possible, hand-over only where it is not, with
   the hand-over clearly marked.

## Recommendation

Option 3, starting with injection for Kubernetes API tokens and Git over
HTTPS (the most asked-for), and hand-over as a last resort that a person
must enable per credential *and* per harness, rated high risk.

This changes the project's core rule, so it waits for the maintainer.
