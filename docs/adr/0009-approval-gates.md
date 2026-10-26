# ADR-0009: Approval gates hold requests at the egress proxy

- Status: accepted
- Date: 2026-10-26

## Context

`spec.approvals.requireFor` promises that the agent asks before certain
actions (pushing code, deleting things). A promise nothing enforces is
worse than none. The agent runs in a VM we do not trust, so the check
cannot live inside it. The one place that sees what the agent does to
the outside world is the egress proxy (ADR-0004).

## Decision

- **Only what the proxy can see.** Actions are a fixed set derived from
  HTTP requests: `http.get|head|post|put|patch|delete|other`, and
  `git.push` (a smart-HTTP `git-receive-pack`), with the groups
  `http.*` and `git.*` (`policy.ApprovalActions`). A runner rejects a
  harness that asks for anything else, so nobody reads "asks you before
  X" for an X that would go unnoticed.
- **Nothing goes unseen.** With approvals, the proxy terminates TLS for
  every allowed host with the job's CA (name-constrained to exactly those
  hosts and wildcards), not only for credentialed ones. Traffic that is
  not TLS-wrapped HTTP then fails.
- **Held before it leaves.** A request whose actions need approval waits
  at the proxy, before anything reaches the upstream; the job shows
  `awaiting_approval`. The person sees method, host and path, and for a
  push every ref update (a push whose updates cannot all be shown is
  refused outright). Approved: it goes on unchanged. Denied, unanswered
  within `approvalTimeout` (default 30 minutes), or its job ended: the
  agent gets 403.
- **Bounded.** At most 8 requests per job wait at once; the job's time
  limit keeps running while it waits. The model endpoint is never gated
  (the budget covers it).
- **Decided anywhere.** `hcc approvals` / `approve` / `deny`, and a banner
  on every page of the web UI. What is shown comes from the agent and is
  displayed as inert text.

## Consequences

- Gates see HTTP(S) only; that is also all a gated harness can use.
- Inspected hosts see the job CA, which the guest trusts; tools pinning
  certificates fail for those hosts.
- Who decided is logged by the runner, not authenticated beyond access
  to the runner socket (or the Control Center session).
