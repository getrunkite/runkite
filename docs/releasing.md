# Releasing Runkite

Version lives in [`VERSION`](../VERSION). Keep Python (`pyproject.toml` /
`__version__`), npm (`package.json` and owned lockfile entries), Helm chart
version and `appVersion`, and the git tag
`v$VERSION` in sync. Public Runkite install commands normally stay unpinned
(`pip install runkite-runner`, `npm install -g runkite-runner`, image `:latest`).
Framework compatibility pins such as `ai@7.0.155` are separate tested contracts.
Release availability labels still need review on every feature release; a badge
cannot make a checkout-only adapter part of an older registry artifact.

## v0.10.0 candidate (unpublished)

The prepared next minor is 0.10.0 because assignment identity protocol 1 and
record-bound SQL finalization require a coordinated upgrade. Version metadata
identifies the candidate; no tag, registry version, image or release is established
by that preparation. Published v0.9.0 remains the verified release below.

Use the [candidate checklist](internal/release-candidate-v0100.md) before tagging.
It records the exact locally qualified source, outstanding fresh-version artifact
and hosted example gates, publication authority and verification requirements.
The upgrade requires stopped writers, restorable backups, Postgres/SQLite
migration 23 or MySQL migration 24, matching planes/runners/custom integrations,
and explicit reconciliation of legacy accounting, hook claims and receipts.
There is no automatic downgrade or missing-proof compatibility path. Managed
policy reviews and app-owned continuations have different invalidation contracts;
follow the linked operator instructions before restoring callers.

The version-scoped notice in [the GoReleaser header](../.goreleaser.yaml) places
the 0.10.0 coordinated-upgrade warning in the generated GitHub release body.
Keep it with the download/install instructions; an automatic commit changelog
alone is insufficient upgrade guidance. Validate the rendered notice before
publication, and keep the historical v0.9.0 verification unchanged.

Before the final package build, change candidate-only package README wording to
describe the exact version that will ship. Keep website release badges and
published availability labels at v0.9.0 until every advertised destination is
verified. Examples that remain checkout assets must retain that distinction
after a release; a new runner wheel or npm tarball does not install them.

<a id="v090-candidate-unreleased"></a>

## v0.9.0 release verification

v0.9.0 is the verified coordinated release for the control-plane binaries, four
registry packages and three image repositories. A version bump or GitHub Release
alone does not establish every destination. Verify the actual tagged archives,
package distributions and both runtime platforms of each image before promoting
availability labels. Do not replace or move any v0.8.0 artifact.

This release adds stable run IDs to the existing public MCP agent tools,
direct navigation to an existing approval record, a bounded plaintext assistant
answer preview beside raw output, and a scripted model mode in the existing
app-owned AI SDK governance example. The example remains a checkout asset;
the model is scripted, while decisions, review and continuation use the real
self-hosted plane. No SQL migration or Runner Protocol change is introduced.
See the [engineering record](internal/mcp-retry-and-operator-flow.md).
The [publication verification record](internal/release-verification-v090.md)
binds the immutable source, published artifacts, actual authentication paths
and qualification limits.

Retain the release's qualification evidence for:

- MCP discovery and real protocol calls with stable IDs: matching terminal and
  in-flight retries, concurrent first calls with explicit or omitted threads,
  conflicting input/agent/thread, current HTTP authentication, tenant isolation,
  delivery cancellation and pre-dispatch cleanup. Include the shared REST retry
  regressions. A retry must not dispatch another assignment or report a
  nonterminal run as success.
- The actual Admin production bundle: exact approval navigation with opaque IDs,
  unchanged tenant Review routing, managed versus app-owned approval wording,
  plaintext/escaped answer previews with unsupported shapes left raw, and an
  event pane that follows its own viewport without scrolling the run page.
- The [scripted walkthrough](../examples/governance_ai_sdk/DEMO.md) with a real
  isolated SQL plane and app store: pending, review, exact continuation, repeated
  continuation, deny and unavailable plane. It must need no model-provider key;
  ordinary example startup must retain its real-provider default.
- The normal version, OpenAPI, package-artifact, Go, SDK, UI and documentation
  gates. Confirm the release archives embed the rebuilt Admin assets. These
  source checks do not establish registry publication or a live-provider soak.
- The checkout-only [Trinity specialist example](../examples/trinity_specialist_mcp/README.md):
  its dedicated workflow checks pinned upstream configuration, the built stdio
  caller, real Runkite API/runner interoperability and supervisor cleanup. Keep
  native Trinity runtime qualification and upstream acceptance claims separate.

Promote the verified version in README, the landing page, support/feature map and
SDK guides together only after publication verification. Package READMEs are
embedded in immutable artifacts; the expanded governance installation/operator
instructions ship in the v0.9.0 package releases. Preserve the
[v0.8.0 publication record](internal/release-recovery-v080.md#completed-recovery)
as historical evidence, including its package-specific authentication paths.

## v0.8.0 release verification

v0.8.0 distributes the identity and usage controls, matching runner packages,
and the first independent npm/PyPI `runkite-governance` packages. Follow the
[coordinated upgrade](ops-runbook.md#identity-and-distribution-upgrade-v080).
Actual artifact verification and the observed publishing-authentication path
are separate evidence; an existing matching package does not prove a fresh
OIDC publication. Keep independent assessment and production-soak status in the
[qualification record](internal/identity-distribution-qualification.md).

The immutable `v0.8.0` runtime source is commit
`c63b8203123f6b7d032b0b5aca59abe9227fd973`. The package-only recovery controller
was corrected after that tag. It uses the original source for npm packages and
verifies the original retained PyPI files without uploading them again. Go
release and container publication remain outside that recovery path. Track the
post-tag controller fixes separately from the tagged runtime in `CHANGELOG.md`
and retain both source and controller commits in each recovery receipt. See the
[recovery procedure](#recover-an-existing-release-without-moving-its-tag).

The [completed publication record](internal/release-recovery-v080.md#completed-recovery)
identifies the actual authentication paths: both npm packages used the
maintainer's personal session, the PyPI helper used its Trusted Publisher, and
the PyPI runner used its project token. The final matching-existing recovery
checks did not exercise npm OIDC. Validate package-specific authority before
the next genuine automated npm publication.

<a id="prepared-v070-candidate"></a>

## v0.7.0 release verification

The coordinated source metadata and tag identify v0.7.0. Earlier v0.6.1 artifacts
do not include the new security controls. Governance helpers were not published with that release. The subsequent
helper-distribution work first ships them in v0.8.0; it does not retroactively
add them to 0.7.0.

A tag starts publication; it does not prove every destination succeeded. Verify
the Release workflow's version check, GoReleaser, Docker, PyPI and npm job
outcomes, then inspect the actual versioned artifacts before announcing them.
Check the binary version and checksums, package metadata/entrypoints and both
image platforms. An optional publication job that skipped is not a registry pass.
Do not infer that `latest` moved merely because a tag or GitHub Release exists.

The [local qualification record](internal/security-completion-qualification.md)
identifies its exact tested images and scope. Those pre-release image IDs remain
historical evidence; final tagged artifacts have their own identities.
Keep the coordinated SQL upgrade instructions in the
[operations runbook](ops-runbook.md#security-completion-upgrade) with the release
notes. The chart defaults to mutable `latest`; pin both the control-plane and
runner image tags to `0.7.0` for this upgrade. Source builds must use matching
plane and runner revisions as well.

## Version consistency gate

Run with Python 3.11 or newer:

```bash
python3 scripts/check-release-version.py
python3 scripts/check-release-version.py --tag "v$(cat VERSION)"
python3 python/tests/test_release_version.py -v
```

The first command checks the coordinated package and chart metadata against
`VERSION`, including file-linked example lock entries. The second also checks
the proposed tag. The release workflow requires this gate before any artifact
publishing job. GoReleaser takes its version from the tag, while npm and Python
builds take theirs from package metadata. Without this check, one tag could
publish artifacts with different versions. Fix mismatches in source rather than
republishing an old version.
Private example/Admin versions, framework dependency pins and API/protocol
schema versions are independent contracts and are not changed by this gate.

`make build` normally identifies development builds with `git describe`. Use
`make build VERSION="$(cat VERSION)"` when inspecting an explicitly versioned local
candidate. This does not create a tag or publish an artifact.

## What a tag publishes

Pushing `v*` runs [`.github/workflows/release.yml`](../.github/workflows/release.yml):

| Artifact | Destination | Requires |
|----------|-------------|----------|
| `runkite` binaries + checksums + OpenAPI JSON | GitHub Release | `GITHUB_TOKEN` (automatic) |
| Control-plane + runner images (`:<version>` and `:latest`) | `ghcr.io/getrunkite/*` | `packages: write` (automatic) |
| `runkite-runner` wheel and sdist | PyPI | **`PYPI_API_TOKEN`** |
| `runkite-governance` wheel and sdist | PyPI | PyPI Trusted Publisher, initially a pending publisher |
| `runkite-runner` npm package | npmjs.com | Package Trusted Publisher; else **`NPM_TOKEN`** |
| `runkite-governance` npm package | npmjs.com | Package Trusted Publisher; else **`NPM_GOVERNANCE_TOKEN`** |

The governance distributions first ship in v0.8.0. Published 0.7.0 used the
earlier runner-only workflow. Before each new tag, verify each package's
registry ownership and authentication setup below. A missing publishing
credential fails the job when files remain unpublished, rather than becoming a
green optional skip. Advertise only versions whose actual registry distributions
have passed verification.

## Recover an existing release without moving its tag

Use the manual **Release** workflow from a reviewed `main` controller when an
existing release needs a workflow correction. Rerunning an old tag reuses that
tag's workflow and scripts, so it cannot pick up a later packaging fix.
The controller validates the existing tag, the original Release run and its
successful version-check/GoReleaser jobs, and the published GitHub Release.
It then checks the original source's coordinated version at that exact commit.
A failed overall release run is acceptable only when those required source and
Go publication checks succeeded.

The manual inputs are `recovery_tag` and `source_run_id`. They identify the
existing release and its original retained artifacts, rather than a new version.
The controller accepts stable `vMAJOR.MINOR.PATCH` tags.
After personal publication of a retained tarball, dispatch a fresh recovery
with the same original `source_run_id`. Its new run ID keeps earlier artifacts
and outcomes intact. Do not substitute a manual recovery run as the source;
the guard requires the original tag-push Release run.
For the first v0.8.0 package recovery, after merging and checking the corrected
controller on `main`:

```sh
gh workflow run release.yml --repo getrunkite/runkite --ref main \
  -f recovery_tag=v0.8.0 -f source_run_id=43695507859
```

This dispatch is authorized to publish missing npm packages. It keeps Go
releases, version tags and container publication outside the recovery path.
The package jobs use two different recovery contracts:

- **PyPI is verify-only.** Download the original `pypi-<package>-<tag>` artifacts
  from the validated source run, validate each manifest's package/version and
  compare the retained wheel and source distribution against actual public
  downloads. The recovery job has no upload credential or OIDC permission. A
  missing retained artifact, missing public file or content mismatch fails the
  job. Do not replace that evidence with a rebuild.
- **npm uses the original source.** Check out the validated tag commit in a
  separate directory and run its locked dependencies, package builds and helper
  integration tests. Use the corrected controller's artifact checker to build
  and pack once, then assert package identity and version before any upload.
  Existing matching tarballs are verified; missing ones use the same OIDC and
  scoped fallback sequence as a normal release. Retain the exact tarball,
  integrity manifest and `source.json`, including both source and controller
  commits. Publication hooks remain disabled.

The original [v0.8.0 Release run](https://github.com/getrunkite/runkite/actions/runs/43695507859)
uploaded both PyPI packages successfully: the runner used its project token and
the helper used OIDC. All four distribution uploads returned HTTP 200. The
following public-index checks still reported both files missing at their
30-second bound. A later verification must match the original retained
manifests; do not repeat those uploads merely because the job is red.
The two npm jobs stopped before publication because npm 12's `pack --json`
returns a package-keyed object while the original checker expected an array.
The corrected controller accepts the supported JSON shapes and retains the
same immutable package-source checks.

PyPI post-upload visibility checks now wait up to 300 seconds for explicit
metadata absence or missing files, while asking intermediaries to revalidate.
Authentication/network errors, malformed responses and content mismatches still
fail immediately. The longer bound does not turn a successful upload response
into proof of public availability. Inspect every recovery job and its retained
receipts before announcing registry installation commands. A matching existing
package is verification evidence, not another OIDC publication.

## Apache-2.0 transition

Current source and v0.6.1+ artifacts use Apache-2.0. Published v0.6.0 artifacts retain their shipped
BUSL-1.1 license. Do not replace old release files, move old tags or relabel
existing registry versions. See the
[transition record](internal/apache-2.0-transition.md) for scope and rationale.

When cutting an Apache-licensed release:

- Inspect the built Go release archives for the current `LICENSE` and `NOTICE`.
- Inspect npm tarballs, Python wheels and source distributions for Apache-2.0
  metadata and the applicable `LICENSE` and `NOTICE` files. Keep third-party
  license and attribution notices intact.
- Verify the final control-plane and runner images carry the license and notice
  files and identify Apache-2.0 in their OCI license metadata.
- Call out the license transition in the release notes, then align the site,
  README and package documentation with the artifacts actually published.
- Keep governance helper qualification separate from publication. Registry
  availability changes only after the coordinated helper jobs actually publish
  and verify the new distributions.

## Documentation and embedded UI release checks

- Keep `CHANGELOG.md` Unreleased items separate from published releases. Move
  an item only when its source and artifact are included in the new release.
- Before the final artifact build, give package READMEs durable registry and
  source-install instructions. Those READMEs ship inside immutable registry
  artifacts. Avoid wording that would still call the package unreleased after
  a successful upload. Promote website and SDK availability labels only after
  the corresponding registry publication has been verified.
- Check the README, `docs/client-sdk.md`, `docs/runners.md`, integration examples
  and `site/support/sdk-integration.html` for matching framework/package status.
- Keep managed execution and app-owned Decide distinct in product, support and
  Admin copy. Do not imply that tool middleware creates runs, that Admin approval
  resumes a caller automatically, or that cancellation undoes external effects.
- Rebuild Admin with `cd admin-ui && npm ci && npm run build` after source edits.
  Commit the resulting `internal/adminui/dist` assets with the source; the binary
  embeds those assets, not the Vite development tree. Run UI tests and lint.
- Check local links in changed Markdown/HTML and compile the Go binary when
  embedded assets change. Historical engineering notes should link to current
  contracts rather than present old behavior as current implementation.
- A push to `main` touching `site/**` triggers Pages independently of a tag.
  Keep availability labels aligned with the artifacts that actually ship. Protocol
  documentation has its own mirror workflow; inspect it when changing that
  contract. These checks do not authorize a deployment or publication.

The main CI check named `test` summarizes the independent `Full test suite` and
`Admin dependency audit` jobs. Both must succeed. This preserves the existing
required check while allowing functional qualification to finish when the audit
finds an unresolved dependency issue. A green suite alongside a red audit or red
summary is incomplete qualification. An advisory exception requires its own
explicit risk decision; the job split does not grant one.

The exact Admin-only picomatch exception is active after verified full hosted
qualification at `47534f3` and expires at `2026-11-25T00:00:00Z`. It retains five
HIGH findings as accepted build-tool risk. Follow the
[qualification and revocation record](internal/release-candidate-v0100.md#qualified-admin-advisory-exception)
before release. It cannot waive the current suite, another finding, changed
inputs, manual qualification or artifact gates, and it must not auto-renew.

## Maintainer checklist (cut a release)

1. [ ] Bump `VERSION` and matching package/Helm fields; update `CHANGELOG.md` and
   run the version consistency gate, including the intended tag.
2. [ ] Enrich `python/README.md` / `typescript/runkite-runner/README.md` if the PyPI/npm page should change (they ship with the next upload).
3. [ ] CI green on `main`.
4. [ ] Tag and push:
   ```bash
   git checkout main && git pull
   git tag -a "v$(tr -d '[:space:]' < VERSION)" -m "v$(tr -d '[:space:]' < VERSION)"
   git push origin "v$(tr -d '[:space:]' < VERSION)"
   ```
5. [ ] Watch **Release**; verify GitHub Release, `docker pull …:latest`, `pip` / `npm` latest.
6. [ ] After first GHCR push: set each package visibility to **Public** (org → Packages).
7. [ ] Announce on HN / Reddit from a **local** draft only. `docs/announce*.md` is gitignored — do not `git add` Show HN copy. After you post, the public record is the thread, not a file in this repo.

## One-time setup (humans)

### PyPI

Keep the existing `runkite-runner` project-scoped `PYPI_API_TOKEN` repository
secret. Its credential check and password-based publish step apply only to
missing runner distributions. The independent governance helper uses OIDC,
without a password or an account-wide API token.

For the first `runkite-governance` upload, the maintainer must register a
[pending trusted publisher](https://docs.pypi.org/trusted-publishers/creating-a-project-through-oidc/)
under the personal PyPI account's **Publishing** settings:

- PyPI project name: `runkite-governance`.
- GitHub owner: `getrunkite`.
- Repository: `runkite`.
- Workflow filename: `release.yml`.
- Environment: leave empty. The current `pypi` job has no GitHub environment.

The job grants only `contents: read` and `id-token: write` and uses the pinned
PyPA publish action with no password for governance. If a GitHub environment is
introduced later, configure the same environment on the PyPI publisher before
publishing. See [PyPI's publishing contract](https://docs.pypi.org/trusted-publishers/using-a-publisher/).

A pending publisher does not create a project or reserve its name. Its first
successful upload creates the project and converts it to a normal publisher;
another account taking the name first invalidates that setup. Registration and
workflow preparation do not prove a successful OIDC publication or helper
availability. Verify the actual uploaded files before changing those claims.
Never put credentials in source or chat.

The workflow builds once with pinned `python/requirements-build.txt` tools,
verifies installed helper behavior and checks each existing distribution against
the intended file contents. It stages and publishes only missing files. Container
timestamps may differ on rebuild, but every regular file and executable bit must
match. A registry/auth failure or content mismatch stops recovery. Publication
requires both the wheel and source distribution to be present and verified.
The job summary records the selected publish step outcomes and the separate
artifact verification outcome. A matching version found before upload is
recovery evidence, not a new OIDC publication. An OIDC or registry failure fails
the job; there is no helper API-token fallback.

### npm Trusted Publisher (preferred)

The workflow installs npm 12.2.2 explicitly before building and publishing.
Node 22 can supply npm 10, which does not implement trusted publishing. npm
requires at least CLI 11.5.1 and Node 22.14.0; the pinned client itself requires
Node 22.22.2, Node 24.15.0 or Node 26+. Keep that engine requirement in mind when
changing the publishing runner. See the [npm trusted-publishing contract](https://docs.npmjs.com/trusted-publishers/).

Passkey / WebAuthn accounts cannot use `--otp=` in CI. After the package exists:

1. For each package, open its npm **Settings**, then **Trusted Publisher**.
   Configure `runkite-runner` and `runkite-governance` independently.
2. GitHub Actions: org `getrunkite`, repo `runkite`, workflow filename `release.yml`, allow `npm publish`
3. Future tags publish via OIDC (`id-token: write` in the Release workflow)

Create each connection when a genuine package publication is ready. npm's
[trusted-publisher expiry rule](https://docs.npmjs.com/trusted-publishers/#trusted-publisher-configuration-expiry)
requires a newly added configuration to complete its first successful publish
within two days. Otherwise it expires; delete and recreate the connection
before the next genuine publish. Verifying an already matching registry version
does not exercise OIDC or validate a new connection. Do not publish dummy
versions to satisfy this setup requirement.

**First-ever publish** (or emergency) may require interactive authentication
through the maintainer's personal npm account. Obtain the exact intended,
verified tarball and its manifest from the coordinated release build first.
Confirm its checksum matches the manifest, then sign in with `npm login` and
publish that explicit artifact:

```sh
npm publish /absolute/path/to/verified-package.tgz --access public --ignore-scripts --registry https://registry.npmjs.org
```

Complete the browser or passkey prompt if requested. Do not use bare
`npm publish` from a checkout, rebuild during publication or substitute another
tarball for the intended version. Verify the registry artifact against the same
manifest after upload before marking it available. A login or configured secret
does not prove package-creation authority, helper ownership or a working Trusted
Publisher.

**Fallback:** use a package-scoped granular token with **Bypass 2FA**. The runner
uses `NPM_TOKEN`; the helper uses `NPM_GOVERNANCE_TOKEN`. Package ownership,
first-publication authorization and trusted-publisher configuration require the
maintainer's registry account. They are not established by this code change.

If a tag already published its binaries or other packages before npm failed,
preserve the tag and successful artifacts. Confirm whether the exact npm version
exists and matches before retrying. A maintainer can retrieve the retained
verified tarball and manifest, sign in using the personal publishing account and
publish that exact tarball with `npm publish ./package.tgz --access public
--ignore-scripts`. Do not rebuild through a publication hook. If the old tag's
workflow remains valid, rerun only the failed npm job. If its packaging code
needs correction, use the [manual recovery controller](#recover-an-existing-release-without-moving-its-tag)
against the existing tag and source run instead. Its public lookup checks both
SHA512 integrity and the downloaded tarball's SHA256 before recognizing a
matching version. Only explicit metadata
HTTP 404 means absent. Authentication failures, network errors, malformed
responses and mismatches stop the job. A lost acknowledgement is reconciled
before the scoped token fallback is attempted. The workflow summary records the
actual OIDC and fallback step outcomes. An already existing matching version does
not qualify as a new OIDC publication. Editing the workflow on `main` does not
change an old tag's workflow. Do not move the tag or overwrite release assets to
repair an authentication failure.

### Local helper artifact and recovery checks

Use Python 3.12 with the pinned build tools, the tested optional Microsoft core,
Node 22 or newer, and the existing locked example dependencies:

```sh
python -m pip install -r python/requirements-build.txt 'agent-framework-core==1.23.0'
python scripts/test_release_artifacts.py -v
python python/tests/test_governance_package_artifacts.py -v
cd typescript/runkite-governance && npm ci && cd ../..
cd examples/governance_ai_sdk && npm ci && cd ../..
python scripts/release_artifacts.py build npm typescript/runkite-governance /tmp/runkite-governance-npm
node scripts/verify_governance_npm.mjs /tmp/runkite-governance-npm/runkite-governance-$(cat VERSION).tgz
```

The output directory must be new or empty. These commands build, install locally
and test; they do not publish. HTTP fixtures listen on loopback. An environment
that prohibits listening cannot qualify those behavior cases and must rerun them
where loopback is permitted. Package source is copied before building Python, so
no checkout build output can satisfy the installed consumer checks.

The release workflow retains intended tarballs/distributions and `manifest.json`
for 30 days. Preserve those files when recovering a partial publication. Do not
rerun successful GoReleaser jobs just to repair a package upload or replace
existing release assets. A source mismatch needs a new version and tag. See the
[engineering contract](internal/governance-distribution.md) for archive comparison,
first-publication gates and the decision-only boundary.

### GHCR

Make `runkite`, `runkite-runner`, and `runkite-runner-ts` **Public** under the org so anonymous `docker pull` works.

### Runner Protocol mirror

The separate `Publish runner-protocol` workflow copies the canonical subtree
to `getrunkite/runner-protocol`. It does not publish Runkite binaries or packages.
Its fine-grained personal-account token needs resource owner `getrunkite`, only
repository `runner-protocol`, and Contents read/write. Metadata read access is
required automatically. No Actions, Secrets or organization-admin permission is
needed. Workflows write is unnecessary while the subtree has no workflow files.

Store the token only as the source repository's Actions secret
`RUNNER_PROTOCOL_MIRROR_TOKEN`. Choose an expiry according to the owner's
credential policy and replace it before expiration. Never paste credentials into
issues, chat, release notes or logs. An invalid-token failure needs credential
rotation, not a protocol source edit or a disabled check.

After rotation, start a fresh manual mirror run so it reads the new secret.
Verify the authenticated subtree push succeeded and compare the destination's
root tree with the dispatched source commit's `runner-protocol` tree. Their
commit IDs differ because subtree splitting creates separate history.
