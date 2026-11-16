# ADR-0018: Runner settings are changed in the Control Center, within bounds

- Status: accepted
- Date: 2026-11-16

## Context

A runner's `runner.yaml` holds two kinds of things. Some belong to the
installer: paths, the sandbox provider, images, VM identities, the
helper, the network. Others are choices a person makes: how many jobs
run at once, how big a job's disk may be, how long finished jobs are
kept, how long an approval waits, when schedules pause, what models
cost. Until now both could only be changed by editing the file as root.
Also, updating a runner from the Control Center wrote the file again, so
any edits were lost.

Our users are mostly newcomers, so every setting must be changeable in
the Control Center. Some of these settings are limits that keep
unattended agents in check. The UI must not become an easy way to loosen
them.

## Decision

- **The Control Center changes the person's settings only**:
  `maxConcurrent`, `maxPluginVMs`, `maxDiskMiB`, `retention`,
  `approvalTimeout`, `minScheduleInterval`, `contactTimeout` and
  `pricing`. The installer's settings stay out of the UI.
- **Reading goes through the runner** (`GET /v1/settings`): the settings
  it runs with, and the bounds its host allows. Reading needs no sudo.
- **Changes are made where the file is, as its owner**, the same way as
  credentials (ADR-0017): `hcc runner settings set`, with the settings
  as JSON on stdin. On a server this runs with `sudo` over the user's
  ssh. On the Mac the file is the user's own, and the Control Center
  writes only its own runner's file, never a path a socket names. The
  command edits only these keys and keeps the rest of the file and its
  comments. It validates the whole result as the runner would. Then it
  replaces the file in one rename, with the old file's mode and owner.
  An invalid change changes nothing.
- **Bounds that hcc enforces itself** apply on top of the file's own
  validation:
  - jobs and plugin VMs at once: at most the host's CPU count;
  - `contactTimeout: off` (schedules never pause for want of a person)
    can only be set in the file;
  - a value already above its bound may stay as it is, but cannot grow.
  The runner's host enforces these bounds, not the browser.
- **Saving restarts the runner** (systemd, or launchd on the Mac), so the
  settings it reports are the ones it runs with. As with an update, a
  save is refused while jobs are running or queued.
- **An update keeps the settings.** `install-runner.sh` reads them before
  it writes `runner.yaml` again and puts them back afterwards
  (`--restore`, checked only against what the file allows). If they no
  longer fit, it says so and uses the defaults.
- **Network access is not a setting here.** On a server it needs a
  sandbox subnet, and the credential broker depends on it. Whether a
  harness may use the network is that harness's own grant, already shown
  in `hcc explain` and the risk rating.

## Consequences

- Newcomers can tune a runner without a shell. The limits that matter
  most for safety cannot be loosened past fixed bounds from a browser
  session.
- A settings change ends nothing that is running, but it does wait until
  the runner is idle.
- Whoever has sudo on the server can still set anything the file
  allows, by editing it.
