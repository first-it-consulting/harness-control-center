# ADR-0017: Credentials are managed in the Control Center, write-only

- Status: accepted
- Date: 2026-10-27

## Context

A credential (an API key with the hosts it may go to and the header it
goes in) lives on the runner, in the credential broker's store
(`credentials.yaml` plus one file per secret). The agent never sees it:
the egress proxy adds it to the agent's requests for those hosts. Until
now the only way to add one was editing that store by hand, with
`sudoedit` on a server. Our users are mostly newcomers: everything they
configure must be possible in the Control Center.

On a server the store is root's and readable only by the runner's group
on purpose. The runner, the part closest to the agents, must not be able
to change where secrets go. That must stay true.

## Decision

- **The Control Center adds, changes and removes credentials.** A secret
  is write-only. It is typed into the UI once and is never shown, listed
  or sent back again, by the UI or the runner. Changing only the hosts or
  the header keeps the secret.
- **Listing goes through the runner** (`GET /v1/credentials`). It returns
  the entries without their secrets and without the secret files' paths.
- **Changes are made where the store is, as its owner**, by
  `hcc credentials set|remove`. That command checks the whole store as
  the runner will before it switches to the new version. The secret
  arrives as JSON on stdin, never on a command line. Each secret goes into
  a new file with the store's mode and group; the store switches to it
  with one rename, and the old file is deleted afterwards.
  - **On a server**, the Control Center runs it with `sudo` over the
    user's own ssh connection. If sudo needs a password, the UI asks for
    it for that one change. The Control Center passes it to `sudo -S`
    on stdin and keeps it nowhere: not in memory after the call, not in
    logs, not on disk. The runner's own user still cannot change the
    store.
  - **On the Mac**, the store is the user's own (`0600`), and the
    Control Center edits it directly.
- **Changes take effect at the next job.** The runner reads the store
  for every job, so nothing restarts.

## Consequences

- Secrets now pass through the Control Center on their way to the
  runner: from the browser on this computer, to the Control Center on
  loopback, then through the user's ssh. The Control Center stores none
  of them. It already only listens on loopback, needs a session for
  every call and checks the origin of every request. A compromised
  Control Center could already drive the runners; now it could also
  capture secrets and the sudo password typed into it at that moment.
- The sudo password crosses the ssh connection, which is encrypted and
  goes to a host the user's ssh config and known_hosts already trust.
- Comments in a hand-edited store are lost at the first change made in
  the UI; the entries themselves are kept.
- Windows can run the Control Center but not a runner, so editing is
  only supported on Linux and macOS.
