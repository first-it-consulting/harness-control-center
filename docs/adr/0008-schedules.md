# ADR-0008: Runners own schedules and loops, and run them cautiously

- Status: accepted
- Date: 2026-10-25

## Context

`spec.trigger` lets a harness start on a cron schedule or repeat in a
loop. Those runs happen without anyone watching, possibly for months, so
a mistake repeats itself. ADR-0003 requires runners to work while the
Control Center is offline.

## Decision

- **The runner owns schedules.** Starting a harness with a trigger adds a
  schedule (`POST /v1/schedules`; `hcc run` and the UI do this), stored in
  the runner's state directory with the exact harness document. The
  runner starts each run as an ordinary job with the harness's own limits.
  Jobs and schedules are separate: a triggered harness is never run
  directly, and an untriggered one never becomes a schedule.
- **Cron** is 5-field, UTC, parsed by our own small parser
  (`internal/cron`, fuzzed against a brute-force reference; no
  dependency). A schedule may not fire more often than the runner's
  `minScheduleInterval` (default 15 minutes, at least 1 minute).
- **No overlap, no catch-up.** A run that is due while the previous one
  is still going is skipped, and the schedule says so. After downtime
  only the next due time counts; missed runs are not started in a burst.
- **Loops** run up to `maxIterations` times, `interval` apart. A loop stops
  at its first run that does not succeed (failed, timed out, over budget,
  cancelled), when a run reports the task complete, or at the limit. The
  agent reports completion by creating `.hcc-done` in its working
  directory; the image's init then prints `HCC-LOOP-DONE` right before its
  result line, after the agent exited. Only that position counts, so the
  agent cannot fake it in its output. A loop whose run was interrupted by
  a runner crash stops.
- **The kill switch pauses every schedule**, and resuming the runner does
  not unpause them: each is resumed on purpose. A normal shutdown does not
  pause them.
- **Explained as unattended.** The explanation says the harness runs
  without anyone watching, and for loops how much all runs together may
  use. The risk rating goes up by one, and the UI's confirmation says
  "run unattended".

## Consequences

- Each loop run starts with an empty working directory; state that must
  carry over lives where the agent may write (e.g. an allowed Git host).
- Times are UTC; the UI shows them in the user's time zone.
- Pausing on lost contact with the Control Center (ADR-0003) is not
  implemented yet.
