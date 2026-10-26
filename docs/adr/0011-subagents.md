# ADR-0011: Subagents are jobs started through the parent's proxy

- Status: accepted
- Date: 2026-10-26

## Context

`spec.permissions.subagents.max` lets an agent hand work to helpers. The
spec promises that a helper gets at most its parent's permissions and
shares its budget. Helpers that run inside the parent's VM (an agent
runtime's own sub-tasks) need no promise: they are the same agent, with
the same rights. A helper with *fewer* rights, though (say, one that
may reach only the package registry while the parent also holds a
GitLab credential), needs a sandbox of its own, and only the runner can
start one.

## Decision

- **Asked for through the proxy.** A job that may start subagents gets
  an egress proxy (it always has one with a model) that also answers
  `/hcc/v1/agents` on its own address, in plain HTTP. The proxy only
  accepts its own guest, so every request is that job's. An agent sees and
  controls only its own helpers: list, start, status with the end of the
  output, stop, results. The `claude-code` image has `hcc-subagent` for
  this, and init tells the agent about it.
- **An ordinary job, checked twice.** A request names a prompt, the
  permissions the helper needs and, optionally, a smaller sandbox. The
  helper's harness gets everything else from the parent: runtime, model,
  target, image, budget and approval gates, plus exactly what reaching
  the model needs. It goes through the same strict parse, defaults and
  validation as any harness, then `policy.CheckChild` (every egress
  rule, credential, tool and the subagent limit within the parent's),
  and the runner's checks like any submission. Its CPUs, memory and disk
  are at most the parent's, and its time at most what the parent has left.
- **One budget, one limit, one lifetime.** The helper's proxy meters the
  parent's budget meter, so the whole tree shares one budget, and
  exhausting it stops the whole tree. Every job's `subagents.max` counts
  the helpers started in its subtree, over the whole run. A helper's
  context derives from its parent's: when the parent ends for any reason,
  its helpers are stopped and queued ones never start. The kill switch
  stops all of them.
- **Queued ahead.** A parent holds a runner slot while it waits, so
  helpers queue ahead of jobs that are not helpers. A helper that never
  gets a slot ends with its parent's time limit.
- **Visible.** A helper's job records its parent; `hcc jobs` and the web
  UI show who started what. The explanation says what a parent may start
  and that helpers stop when it ends.

## Consequences

- Helpers cost a VM each and wait for a free slot (`maxConcurrent`).
- A helper's usage shown is the tree's, since they share one meter.
- What a helper prints and hands back is input to its parent, as
  untrusted as the parent's own work; the parent's limits still apply to
  anything it does with it.
- The API is plain HTTP inside the sandbox's own link. Anyone in the VM
  can use it, which is the agent anyway.
