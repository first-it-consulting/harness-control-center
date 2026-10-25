# ADR-0007: The Control Center is a loopback-only web UI with a login link

- Status: accepted
- Date: 2026-10-25

## Context

Newcomers need a UI, not a CLI. The UI starts agents, so whoever controls
it controls what agents do. A web UI on the user's computer can be reached
by every local process and, through the browser, by every web page the
user visits (CSRF, DNS rebinding). What it shows (console output, file
names, job errors) can be written by an agent, so possibly by a prompt
injection.

## Decision

- **One binary, no build step.** `hcc control serve` serves the UI from
  files embedded in the binary: plain HTML, CSS and one ES module, with no
  frontend dependencies (no npm supply chain) and nothing loaded from
  elsewhere.
- **Loopback only.** It refuses any listen address that is not a loopback
  IP. Remote use goes through `ssh -L`.
- **Login link.** At start it prints a URL with a 256-bit random token.
  Opening it creates a session (HttpOnly, SameSite=Strict cookie, 12 h)
  and redirects away from the token. The token is never put on a command
  line (other local users could read it from the process list).
- **Every request is checked.** The Host header must be this server's
  (no DNS rebinding). The API needs a session. Every change also needs
  the session's CSRF token in a header, a same-origin `Origin` header, and
  a JSON body (a cross-site form cannot send one). Unknown JSON fields
  are refused.
- **Strict CSP and headers.** `default-src 'none'`; scripts, styles and
  images only from the server itself; no inline script; no framing
  (`frame-ancestors 'none'`, `X-Frame-Options: DENY`); `nosniff`; no
  referrer; `no-store`.
- **Agent text is never HTML.** The UI builds its DOM only with
  `textContent` and attributes. A test fails the build if the code uses
  `innerHTML` or similar, or if the page has inline script or styles. The
  server also neutralizes control characters, bidi overrides and invalid
  UTF-8 in the console (`internal/safetext`). Results are offered only as
  a download (`application/octet-stream`, attachment), never rendered.
- **Starting needs a confirmation.** The UI shows the plain-language
  explanation and risk of exactly the document being started, and the
  API refuses a start without `confirmed: true`. It checks the document
  again, and checks that the runner's target matches the harness's.
- **The Control Center decides nothing.** Runners enforce every limit
  themselves. The Control Center holds no secrets; its own files
  (`control.yaml`, saved harnesses) are owner-only.

## Consequences

- One browser profile at a time works smoothly; a new browser needs the
  link again (restart the server for a new one).
- No TLS: traffic never leaves the machine. Over `ssh -L` it is inside
  ssh.
- The "Stop everything" button only reaches runners the Control Center
  can reach; the UI names the ones it could not.
