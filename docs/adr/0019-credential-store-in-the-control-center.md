# ADR-0019: The Control Center keeps the credentials and copies them to runners

- Status: accepted
- Date: 2026-11-17
- Amends: ADR-0017 (the Control Center no longer only passes secrets through)

## Context

Under ADR-0017, every credential lived in one runner's broker store and was
managed per runner. A person with three runners added the same API key
three times, had to know which runner a harness would run on, and could not
see their credentials in one place. They asked for "a little credential
store": define a credential once, attach it to harnesses, and have it
arrive where the harness runs.

What must not change: the agent never sees a secret (the broker adds it to
requests to the credential's hosts only), and the runner's user cannot
change where a secret goes (the broker store on a server stays root's).

## Decision

- **The Control Center keeps the person's credentials** in
  `<config dir>/credentials/`: `credentials.yaml` and one file per secret
  in `secrets/`. The directory is `0700` and the files `0600`, the same
  format and checks as a runner's store (`internal/credentials`). A secret
  is write-only in the UI, as before.
- **A harness attaches credentials by name** (`permissions.credentials`,
  unchanged). In the form, attaching one also allows its hosts.
- **Starting or scheduling a harness copies the credentials it is granted
  to that runner first**, with ADR-0017's path: `hcc credentials set` as
  the store's owner (sudo over ssh on a server, asking for the password if
  sudo needs it; directly on the Mac). Only those credentials, only to
  that runner. The job is submitted only after the copy succeeded.
- **Unchanged credentials are not copied again.** `copies.yaml` records,
  per runner and credential, a SHA-256 of the entry and secret that runner
  got. The credentials page shows each runner's copy as current or older,
  and can copy it again (a schedule uses the runner's copy).
- **Credentials only a runner has** (added there before) keep working; the
  page lists them and can remove them.
- **Only request headers.** A credential is a header for named hosts. A
  password or file the agent itself would read (a database password, a
  kubeconfig) would be visible to the agent; that is a separate decision
  (ADR-0020, proposed), not part of this one.

## Consequences

- The Control Center now holds secrets at rest, owner-only, like SSH keys
  or a CLI's tokens in the user's config directory. Anyone who can read
  the user's files can read them, as with those. It already could drive
  every runner.
- A credential reaches a runner only when a harness granted it runs there
  (or the person copies it), so a runner holds no more than it needs.
- Removing a credential here leaves the runners' copies until they are
  removed there, so a running job is not cut off; the page says so.
- Windows can run the Control Center but cannot keep credentials yet (the
  store's file handling is Unix-only).
