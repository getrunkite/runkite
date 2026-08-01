# Security Policy

## Reporting a Vulnerability

If you find a security vulnerability in Runkite, please **do not open a public GitHub issue**.

Instead, report it privately using one of these channels:

1. **[GitHub Security Advisories](https://github.com/getrunkite/runkite/security/advisories/new)** (preferred) -- lets you report privately and track the fix with maintainers before any public disclosure.
2. **Email**: [sharanharsoor@gmail.com](mailto:sharanharsoor@gmail.com) if you'd rather not use GitHub.

Please include:

- A description of the vulnerability and its potential impact.
- Steps to reproduce, or a proof-of-concept if you have one.
- The affected version/commit.

## Response

This is a solo-maintained project -- there's no formal SLA, but security reports get priority over everything else. Expect an initial acknowledgment within a few days. Once a fix is ready, it will be released and the reporter credited (unless you'd prefer to stay anonymous), with a coordinated disclosure timeline agreed with you first.

## Supported Versions

Only the latest tagged release is actively supported with security fixes. There is no long-term-support branch at this stage.

## Scope

Areas of particular interest for security review:

- Auth providers (`internal/auth/`): JWT/API-key/webhook validation, permission checks, admin-key handling.
- Multi-tenancy isolation (`internal/tenant/`) -- a bug here could leak data across tenants.
- The Runner Protocol's trust model (`internal/bridge/`) -- runner-token validation, and the documented trade-offs of direct-mode DB access (see the README's "Runners" section).
- Webhook signature verification and connector credential handling (`internal/hooks/`, `internal/connector/`).

Known, already-documented limitations (coarse-grained authorization, single-control-plane-node crash recovery, etc.) are listed in the README's "Known Limitations" section -- please check there first in case what you found is already a stated trade-off rather than a new bug.
