# Releasing Runkite

Version lives in [`VERSION`](../VERSION). Keep Python (`pyproject.toml` /
`__version__`), npm (`package.json`), Helm `appVersion`, and the git tag
`v$VERSION` in sync. **Public install docs stay unpinned** (`pip install
runkite-runner`, `npm install -g runkite-runner`, `docker pull …:latest`) so
README/site do not need edits on every bump — badges show the current version.

## What a tag publishes

Pushing `v*` runs [`.github/workflows/release.yml`](../.github/workflows/release.yml):

| Artifact | Destination | Requires |
|----------|-------------|----------|
| `runkite` binaries + checksums + OpenAPI JSON | GitHub Release | `GITHUB_TOKEN` (automatic) |
| Control-plane + runner images (`:<version>` and `:latest`) | `ghcr.io/getrunkite/*` | `packages: write` (automatic) |
| `runkite-runner` wheel | PyPI | repo secret **`PYPI_API_TOKEN`** |
| `runkite-runner` npm package | npmjs.com | **Trusted Publisher (OIDC)** preferred; else **`NPM_TOKEN`** |

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
