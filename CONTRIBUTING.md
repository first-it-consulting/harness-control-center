# Contributing

Thanks for helping! This project lets AI agents act on people's behalf, so
the bar for safety is high. These rules keep it that way.

## Ground rules

1. **Deny by default.** No change may grant an agent anything implicitly.
   New capabilities start disabled and must be shown in `hcc explain`.
2. **Tests come with the code.** Every change includes tests, including
   for the *denied* and *failure* paths. Security-relevant parsing and
   matching code also gets a fuzz test.
3. **One enforcement point.** Permission decisions live in
   `internal/policy`. Other components ask the engine; they never
   re-interpret the spec themselves.
4. **No secrets in sandboxes, logs or errors.** Ever.
5. **Big decisions get an ADR** in `docs/adr/`. If a change alters the
   threat model, update `docs/threat-model.md` in the same PR.

## Workflow

```sh
make check   # lint, tests with the race detector, govulncheck
make fuzz    # 30s per fuzz target; FUZZTIME=5m for longer runs
make build
```

You need Go (the version in `go.mod`) and
[golangci-lint](https://golangci-lint.run/) v2.

- Use [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:` ...). Releases and the changelog are generated
  from them.
- Keep PRs small and focused. Fill in the PR template's safety checklist.
- All GitHub Actions are pinned to a commit SHA. Dependabot keeps them current.

## Code of conduct

Be kind and constructive. Harassment of any kind is not tolerated;
maintainers may remove anyone who does not respect this.
