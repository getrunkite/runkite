# Releasing Runkite

Version lives in [`VERSION`](../VERSION). Keep Python (`pyproject.toml` /
`__version__`), npm (`package.json`), Helm `appVersion`, and the git tag
`v$VERSION` in sync. Public Runkite install commands normally stay unpinned
(`pip install runkite-runner`, `npm install -g runkite-runner`, image `:latest`).
Framework compatibility pins such as `ai@7.0.155` are separate tested contracts.
Release availability labels still need review on every feature release; a badge
cannot make a checkout-only adapter part of an older registry artifact.

## What a tag publishes

Pushing `v*` runs [`.github/workflows/release.yml`](../.github/workflows/release.yml):

| Artifact | Destination | Requires |
|----------|-------------|----------|
| `runkite` binaries + checksums + OpenAPI JSON | GitHub Release | `GITHUB_TOKEN` (automatic) |
| Control-plane + runner images (`:<version>` and `:latest`) | `ghcr.io/getrunkite/*` | `packages: write` (automatic) |
| `runkite-runner` wheel | PyPI | repo secret **`PYPI_API_TOKEN`** |
| `runkite-runner` npm package | npmjs.com | **Trusted Publisher (OIDC)** preferred; else **`NPM_TOKEN`** |

This workflow does not publish `runkite-governance` to npm or PyPI. Its packaging
metadata, local wheel/tarball checks and optional Microsoft middleware do not
change that. Do not describe those helpers as published without a separate,
verified maintainer publication.

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
- Keep governance helper packaging checks separate from publication. This
  transition does not add those helpers to the release workflow.

## Documentation and embedded UI release checks

- Keep `CHANGELOG.md` Unreleased items separate from published releases. Move
  an item only when its source and artifact are included in the new release.
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
  Keep v0.6.0 labels until public artifacts contain the feature. Protocol
  documentation has its own mirror workflow; inspect it when changing that
  contract. These checks do not authorize a deployment or publication.

## Maintainer checklist (cut a release)

1. [ ] Bump `VERSION` and matching package/Helm fields; update `CHANGELOG.md`.
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

Repo secret `PYPI_API_TOKEN` (project-scoped token for `runkite-runner` after first upload is ideal).

### npm — Trusted Publisher (preferred)

Passkey / WebAuthn accounts cannot use `--otp=` in CI. After the package exists:

1. https://www.npmjs.com/package/runkite-runner → **Settings** → **Trusted Publisher**
2. GitHub Actions: org `getrunkite`, repo `runkite`, workflow filename `release.yml`, allow `npm publish`
3. Future tags publish via OIDC (`id-token: write` in the Release workflow)

**First-ever publish** (or emergency): on your laptop, `npm login` then
`npm publish --access public` and complete the browser / passkey prompt.

**Fallback:** granular token with **Bypass 2FA** → secret `NPM_TOKEN` (often still hits `EOTP` for new packages; prefer Trusted Publisher).

### GHCR

Make `runkite`, `runkite-runner`, and `runkite-runner-ts` **Public** under the org so anonymous `docker pull` works.
