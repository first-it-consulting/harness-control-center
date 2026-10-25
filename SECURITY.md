# Security Policy

This project runs autonomous AI agents. A security bug here can mean an
agent acting outside the limits its user approved, so we treat reports as
our highest priority.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through
[GitHub Security Advisories](https://github.com/first-it-consulting/harness-control-center/security/advisories/new).

Please include the affected version (`hcc version`), a description, and
steps or a harness file that reproduces the problem. Remove all secrets first.

We aim to acknowledge reports within 3 working days and to agree on a fix
and disclosure timeline with you. We credit reporters unless they prefer
otherwise.

## In scope

Anything that lets an agent exceed what its harness grants. For example:

- escaping the sandbox, or reaching hosts, ports or networks outside the allowlist
- reading a credential, or using one that was not granted
- a subagent obtaining permissions its parent does not have
- exceeding the time, token or cost budget, or evading the kill switch
- the plain-language explanation or risk rating misrepresenting what a harness may do
- tampering with released artifacts or the release pipeline

## Supported versions

Before 1.0, only the latest release receives security fixes.
