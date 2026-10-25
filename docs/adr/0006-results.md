# ADR-0006: Results leave the sandbox as a sanitized archive on a blank disk

- Status: accepted
- Date: 2026-10-25

## Context

An agent's work (code, reports) must get back to the user. Whatever the
guest hands over was written by the agent, so possibly by a prompt
injection: it is hostile input, and must never be able to make the host
(as root or as the runner) or the user's tools do something unintended.

## Decision

- **A blank disk per job.** The helper creates a sparse `disk.img`
  (`spec.sandbox.diskMiB`, bounded by the runner's `maxDiskMiB`) in the
  root-owned jail, owner-only for the VM's identity, and attaches it as
  the guest's only writable drive. Its full size is reserved against the
  jail filesystem's free space, counting what the other jails' disks may
  still grow.
- **The guest writes a tar.** The image's init archives the agent's
  workspace onto the raw device after the agent ends, and syncs.
- **Root only copies bytes.** After a clean guest shutdown (never after a
  timeout or kill), `hcc-helper collect` opens the disk without following
  links, requires a regular, singly linked file within the size limit, and
  streams it up to the end of its data. Root never mounts, parses or
  extracts anything the guest wrote.
- **The runner re-packs.** Unprivileged, `internal/results` parses the
  stream and writes a new tar: only regular files and directories, clean
  relative names (no `..`, no absolute paths, no control characters,
  bounded length and depth), normalized metadata (root, 0644/0755, epoch
  times, no xattrs, no setuid), bounded total size and file count. Links,
  devices and FIFOs are dropped and listed. A malformed or oversized
  archive is reported on the job and nothing is kept.
- **The user gets a file, not an extraction.** `hcc results JOB -o FILE`
  saves the archive; it never overwrites a file and never writes to a
  terminal. Skipped names are shown with control characters neutralized.

## Consequences

- Results are files only: no symlinks or permissions survive.
- The workspace lives in guest memory; its size is bounded by the VM's
  memory as well as by the disk.
- A disk is collected only when the guest shut itself down, so a run that
  timed out or exceeded its budget returns no results.
- The stored archive is still the agent's content: users should review it
  like any untrusted download before running anything in it.
