# ADR-0010: Tools come from a signed catalog on the runner

- Status: accepted
- Date: 2026-10-26

## Context

`spec.permissions.tools.install` lists tools an agent needs (Python,
Node, a compiler). Letting the agent install them itself would need
root in the VM and a path to package mirrors, and nothing outside the VM
could tell what it installed. The VM's root filesystem is read-only and
it has no network beyond the harness's allowlist.

## Decision

- **The runner offers, the harness picks.** Each image may have a tool
  catalog, built by `install-runner.sh` when it builds the image, from
  `HCC_TOOLS` (default `python3 py3-pip nodejs npm make gcc musl-dev curl
  go`). It holds, for every offered package, Alpine's signed package
  files of it and of each dependency the image lacks, fetched from the
  image's own Alpine release. Each package is test-installed offline at
  build time. The catalog is root's: `tools.ext4` (the files) and
  `tools.txt` (the list the runner reads, `internal/tools`), configured
  as `images.<name>.tools` in `runner.yaml`.
- **Checked before the job exists.** A harness asking for a tool its
  image does not offer, or in another version (`name@version` must match
  exactly), is rejected. Nothing is installed that the user did not see in
  the explanation.
- **Installed before the agent, by init, offline.** The runner passes the
  resolved file names in the task and attaches the catalog read-only
  (`hcc.tools=vdX`). Init, as root and before the agent starts, checks
  every name again, puts memory overlays over the system directories,
  and runs `apk add --no-network` with no repository on exactly those
  files. apk verifies every package's signature against the image's
  keys (a tampered or foreign-signed file is refused). Init then unmounts
  the catalog; its device is root's, so the agent cannot read it. If
  anything fails, the job fails and the agent never starts.
- **The agent stays unprivileged.** It runs as `agent` (uid 1000) and
  cannot install system packages. What it can download (say, from PyPI)
  is limited by its network allowlist like everything else.

## Consequences

- Tools are only as current as the last runner install; reinstalling
  refreshes them (the image is rebuilt when `HCC_TOOLS` or the install
  script changes).
- Installing costs boot time and memory (the overlays live in RAM and
  count against `memoryMiB`).
- Only images with a catalog offer tools: `claude-code` does, `base`
  (tests only) does not.
