# ADR-0012: Schedules pause when nobody has been in contact

- Status: accepted
- Date: 2026-10-26

## Context

Runners keep running schedules while the user's laptop sleeps (ADR-0003),
which is the point. But a schedule nobody looks at any more (the user is
away, has lost access, or forgot about it) keeps spending money and
acting on their behalf. "Stop everything" needs someone who can reach
the runner.

## Decision

- **Contact is any request on the runner's socket.** Only people use it:
  the `hcc` CLI and the Control Center, locally or over ssh. Agents never
  can (they reach only their proxy). A running Control Center contacts
  every runner every 5 minutes.
- **No contact for `contactTimeout`, schedules pause.** The default is
  `72h`, configurable in `runner.yaml` from `1h` to `8760h`, or `off`.
  While contact is lapsed, every active schedule is paused, including ones
  that become active, and no run starts. Running jobs go on with their
  own limits.
- **Contact does not unpause.** As after the kill switch, each schedule
  is resumed on purpose. A schedule's message says why it was paused.
- **Survives restarts.** The time of the last contact is stored in the
  state directory (at most once a minute), so a runner that restarts
  after a long silence does not count its own start as contact.
- **Visible.** The Runners page shows the last contact and the timeout,
  and the explanation of a scheduled harness says that it pauses.

## Consequences

- Someone who only uses schedules and never looks for three days finds
  them paused. That is the intended trade-off; `contactTimeout` adjusts
  it, and `off` restores the old behaviour.
- Monitoring scripts that query the socket count as contact. They should
  only do so on behalf of a person.
