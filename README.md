# Harness Control Center

> **Status: pre-alpha.** The harness spec, policy engine, Firecracker
> sandbox, runner daemon, CLI, egress proxy, credential broker and the
> Claude Code runtime work: an agent runs in a confined microVM, reaches
> exactly the hosts its harness allows, and uses credentials it never
> sees, within a token budget enforced at the proxy. Its files come back
> as a sanitized archive. Harnesses can run on a schedule or in a loop, and
> ask a human before chosen actions (approval gates). The
> web UI (`hcc control serve`) manages harnesses, schedules, runners and
> jobs.
> "Harness Control Center" is a working codename.

Run AI agents on your own machines, safely, without being an expert.

You describe a task in plain words. The Control Center turns it into a
**harness**: the task plus exactly what the agent may do, where it runs,
when it runs, and how much it may spend. A **runner** on your computer or
on your own server starts the agent in a sandbox and stops it when a limit is hit.
You watch every agent, its logs and its spending in one place.

## Safety model, in one paragraph

Every agent starts with **nothing**: no network, no credentials, no helpers,
30 minutes, $1. Everything beyond that is granted explicitly, shown to you in
plain language before it starts, and enforced outside the agent's reach.
Secrets never enter the sandbox. A helper agent can only ever get a subset of its
parent's permissions. Read the [threat model](docs/threat-model.md).

## Try the spec today

```sh
git clone https://github.com/first-it-consulting/harness-control-center
cd harness-control-center
go run ./cmd/hcc explain examples/nightly-dependency-bump.yaml
```

```
Risk: high
  because it can reach the internet
  because it can act with your credentials
  because it combines network access with credentials
  because it runs again and again without anyone watching

• Runs the claude-code agent on server homelab-01.
• Stops after 2h0m0s per run, or once its model has used 2,000,000 tokens (or cost $5.00, for models the runner has a price for).
• When it finishes, it can hand back up to 4,096 MiB of files (plain files and folders only; nothing else is kept).
• Starts automatically on the cron schedule "0 2 * * *" (times in UTC), without anyone watching, until you pause or delete the schedule. A run is skipped while the previous one is still going. Every run has the limits above.
• Can connect only to: gitlab.com, registry.npmjs.org.
• Can act as you using: gitlab-my-team. The agent never sees these secrets.
• Gets these tools installed before it starts: nodejs, npm. They come from the runner's own catalog of signed packages. The agent cannot install system packages itself; what it can download is limited by its network access.
• Cannot start other agents.
• Asks you before: git.push, http.delete. To catch these, every connection it makes is inspected (only HTTPS works then). While it waits, the job pauses but its time limit keeps running; if nobody decides in time (30 minutes by default), the action is refused.
```

## Run it on your own server

With a Linux server that has KVM (a Proxmox VM with nested virtualization
works), see [deploy/README.md](deploy/README.md). Then:

```sh
hcc run --ssh my-server hello.yaml   # explain, confirm, run, follow; Ctrl-C stops the job
hcc jobs --ssh my-server             # list jobs
hcc logs --ssh my-server <job>       # console output
hcc kill --ssh my-server             # kill switch: stop everything, refuse new jobs
hcc resume --ssh my-server
```

The runner listens on no network port. `--ssh` uses your own ssh
configuration and host keys.

## Use the web UI

```sh
hcc control serve
```

It prints a link; open it in your browser. Add your runner under
**Runners** (its ssh host), write or paste a harness under **Harnesses**,
read what it may do, and start it. **Jobs** shows every run with its
console, usage and results; **Stop everything** is the kill switch for all
runners. The UI listens on this computer only; see
[ADR-0007](docs/adr/0007-control-center-web-ui.md).

## Documentation

- [Vision](docs/vision.md): what this is and who it is for
- [Architecture](docs/architecture.md): Control Center, runner, sandbox, plugins
- [Threat model](docs/threat-model.md): what we defend against, and how
- [Decision records](docs/adr/): why things are the way they are

## Verifying releases

Every release is signed keylessly with [Sigstore](https://www.sigstore.dev/)
and carries an SBOM and SLSA build provenance.

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/first-it-consulting/harness-control-center/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
gh attestation verify hcc_*.tar.gz --repo first-it-consulting/harness-control-center
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
