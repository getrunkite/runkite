# Releasing Runkite

Version lives in [`VERSION`](../VERSION). Keep Python (`pyproject.toml` /
`__version__`), npm (`package.json` and owned lockfile entries), Helm chart
version and `appVersion`, and the git tag
`v$VERSION` in sync. Public Runkite install commands normally stay unpinned
(`pip install runkite-runner`, `npm install -g runkite-runner`, image `:latest`).
Framework compatibility pins such as `ai@7.0.155` are separate tested contracts.
Release availability labels still need review on every feature release; a badge
cannot make a checkout-only adapter part of an older registry artifact.

<a id="prepared-v070-candidate"></a>

## v0.7.0 release verification

The coordinated source metadata and tag identify v0.7.0. Earlier v0.6.1 artifacts
do not include the new security controls. Governance helpers were not published with that release. The subsequent
helper-distribution work prepares them for the next coordinated release; it does
not retroactively add them to 0.7.0.

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
| `runkite-governance` wheel and sdist | PyPI | **`PYPI_GOVERNANCE_API_TOKEN`** |
| `runkite-runner` npm package | npmjs.com | Package Trusted Publisher; else **`NPM_TOKEN`** |
| `runkite-governance` npm package | npmjs.com | Package Trusted Publisher; else **`NPM_GOVERNANCE_TOKEN`** |

The governance rows describe the newly prepared workflow, not an already
verified registry release. Published 0.7.0 used the earlier runner-only workflow.
Before a new tag, complete each helper's registry ownership and authentication
setup below. A missing publishing credential is now a failed job when files
remain unpublished, rather than a green optional skip. Do not advertise a helper
until its actual registry distributions have passed verification.

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

Verify ownership of both `runkite-runner` and the independent
`runkite-governance` project. Configure project-scoped repository secrets
`PYPI_API_TOKEN` and `PYPI_GOVERNANCE_API_TOKEN` respectively. An existing
runner-scoped token does not authorize the helper. A new project may need a
maintainer-controlled first upload before a project-scoped token can be created.
That is an external account setup gate, not something package metadata can
authorize. Never put credentials in source or chat.

The workflow builds once with pinned `python/requirements-build.txt` tools,
verifies installed helper behavior and checks each existing distribution against
the intended file contents. It stages and publishes only missing files. Container
timestamps may differ on rebuild, but every regular file and executable bit must
match. A registry/auth failure or content mismatch stops recovery. Publication
requires both the wheel and source distribution to be present and verified.

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

**First-ever publish** (or emergency): on your laptop, `npm login` then
`npm publish --access public` and complete the browser / passkey prompt.

**Fallback:** use a package-scoped granular token with **Bypass 2FA**. The runner
uses `NPM_TOKEN`; the helper uses `NPM_GOVERNANCE_TOKEN`. Package ownership,
first-publication authorization and trusted-publisher configuration require the
maintainer's registry account. They are not established by this code change.

If a tag already published its binaries or other packages before npm failed,
preserve the tag and successful artifacts. Confirm whether the exact npm version
exists and matches before retrying. A maintainer can retrieve the retained
verified tarball and manifest, sign in using the personal publishing account and
publish that exact tarball with `npm publish ./package.tgz --access public
--ignore-scripts`. Do not rebuild through a publication hook. Then rerun only the
failed npm job; its public lookup checks both SHA512 integrity and the downloaded
tarball's SHA256 before recognizing a matching version. Only explicit metadata
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
