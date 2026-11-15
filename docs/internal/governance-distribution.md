# Governance helper distribution and release recovery

Availability: v0.8.0. The packaging and immutable recovery contracts tracked in
[#45](https://github.com/getrunkite/runkite/issues/45) and
[#48](https://github.com/getrunkite/runkite/issues/48) distribute the existing
helpers without introducing a model loop or changing the decision API.
Published Runkite 0.7.0 artifacts did not include these independent npm/PyPI
distributions. Registry publication and live OIDC evidence remain distinct.

## Caller and boundary

An existing Next or Nuxt backend can keep AI SDK execution in its process and
wrap a single-result executor with `createGovernedExecute`. An existing Python
Microsoft Agent Framework app can use `GovernanceFunctionMiddleware` on exact
registered function objects. Neither should need to clone Runkite to install a
supported helper. Both integrations already exist; this work distributes and
qualifies them rather than adding another framework implementation.

Allow, deny, pending, bounded transport calls and the existing argument snapshot
rules remain unchanged. The app persists pending proposals and owns continuation
and side-effect idempotency. Polling does not consume approval. This path creates
no managed thread/run and does not gain run budgets, kill switches, native
approval/resume, streaming tool execution or a sandbox. The Microsoft middleware
retains native mutable execution containers and its documented application
cooperation requirement.

## Operator handoff

The package is installed in the application backend, while authentication and
policy are configured on the existing self-hosted plane. Keeping those steps
separate avoids mistaking an installed helper for enabled governance. Python and
Node applications use the same decision API and policy engine; they need
language-specific clients, not separate control planes.

The [operator walkthrough](../app-owned-governance.md) ties primary
`governance:decide` credentials, SQL pending/audit storage, logical policy names,
Admin grants and approval continuation into one procedure. The app credential is
not a runner token or an independent `auth.admin_keys` entry. A logical
agent/connector name does not require managed registration. Operators review
submitted decisions in Pending/Audit, not Threads/Runs or managed connector
execution evidence. The app remains responsible for saved arguments, ownership
checks and idempotent execution after approval.

## Package contract

- `typescript/runkite-governance` is an independent ESM package with exported
  JavaScript and declarations, Apache LICENSE/NOTICE, Node 22 minimum and no
  runtime npm dependencies. Its six-file tarball is an explicit allowlist.
- `python/runkite_governance` is an independent stdlib client with Python 3.11
  minimum, `py.typed`, Apache LICENSE/NOTICE and optional
  `agent-framework-core==1.23.0`. The base import does not import that SDK.
- The Python sdist is rebuilt into a wheel offline. Both installed forms run
  HTTP/deadline checks and the existing middleware security and real offline
  framework integration tests from temporary consumer directories. A checked
  import origin prevents a source checkout from satisfying the test.
- The npm verifier installs the actual tarball offline outside the checkout,
  checks its import origin and declaration consumption, then reruns the existing
  helper suite and pinned AI SDK streaming/non-streaming examples against it.
  Copied test source is a test harness, not an installed production dependency.

`python/tests/test_governance_package_artifacts.py` checks exact package contents
and uses excluded fixture traps to prevent test/live source from entering the
Python artifacts. `scripts/verify_governance_npm.mjs` checks every npm member and
exercises the public export. A missing SDK dependency or inability to run an HTTP
fixture is a failure, not an automatic qualification skip.

## Build once and recover conservatively

`scripts/release_artifacts.py` never publishes. It builds an artifact set once,
records immutable hashes and every archive member, and queries only public npm
and PyPI HTTPS endpoints. Explicit metadata HTTP 404 means missing; registry
authentication failures, rate limits, malformed responses, redirects and network
errors fail closed. Existing metadata alone is insufficient.

For npm, both registry SHA512 integrity and the downloaded tarball's SHA256 must
match the exact intended tarball. A different existing version fails. For PyPI,
the download must match its registry SHA256 and every regular member must match
the intended artifact's bytes and file permissions. ZIP/gzip/tar container dates
can differ between rebuilds; these dates do not permit changed runtime code,
metadata, notices, additional files or changed file permissions. Links, duplicate
members, traversal paths and excessive archives are rejected. Local artifacts
are checked again against their recorded manifests before any registry check.

The release workflow publishes the verified npm tarball with lifecycle scripts
disabled. Its OIDC step has no repository fallback token and its actual failure
remains visible. After a failed attempt, a read-only reconciliation handles a
lost acknowledgement before any fallback publication. An already matching
version proves recovery only, not that a new OIDC publish worked. PyPI recovery
stages only missing files after checking all existing distributions. It never
uses an unchecked `skip-existing` option. Intended artifacts and manifests are
retained as workflow artifacts for 30 days.

This is not a registry, provenance signer or replacement package manager. The
registry remains the publication authority. The checker bounds metadata,
downloads and unpacked content, does not send publishing credentials in public
lookups, and does not print remote response bodies. A real source correction
requires a new version. Do not move tags, delete successful assets or overwrite
an existing package to make recovery pass.

## Qualification and first publication

CI runs recovery failure cases, both installed helper contracts and the existing
SDK suites. Build tooling is pinned separately in
`python/requirements-build.txt`. The runner and helper versions remain
coordinated by `scripts/check-release-version.py`.

Package READMEs give separate installation instructions for a verified registry
release and for unpublished source. They avoid freezing temporary candidate
availability into an immutable published artifact. The website and source
handbook carry the observed publication status and are promoted only after
registry verification. Changing an already published README payload would
require a new package version, just like a runtime correction.

The helper names, publishing account ownership, trusted-publisher configuration
and project-scoped fallback credentials must be verified by the maintainer before
a genuine release. The new helper jobs fail if publication remains necessary but
no usable credential exists. They do not silently skip and advertise success.
See [releasing](../releasing.md) for exact account-side gates. No fabricated
version or already-existing release can qualify first-time OIDC publication.
