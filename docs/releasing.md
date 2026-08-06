# Releasing Runkite

Version lives in [`VERSION`](../VERSION) (currently **0.1.0**). Python and npm package versions must match that file and the git tag `v$VERSION`.

## What a tag publishes

Pushing `v*` runs [`.github/workflows/release.yml`](../.github/workflows/release.yml):

| Artifact | Destination | Requires |
|----------|-------------|----------|
| `runkite` binaries + checksums + OpenAPI JSON | GitHub Release | `GITHUB_TOKEN` (automatic) |
| Control-plane + runner images | `ghcr.io/getrunkite/*` | `packages: write` (automatic) |
| `runkite-runner` wheel | PyPI | repo secret **`PYPI_API_TOKEN`** |
| `runkite-runner` npm package | npmjs.com | repo secret **`NPM_TOKEN`** |

Without PyPI/npm secrets, the release still publishes binaries + GHCR; those jobs print a skip notice.

## Maintainer checklist (cut a release)

1. [ ] `VERSION`, `python/pyproject.toml`, `python/runkite_runner/__init__.py`, `typescript/runkite-runner/package.json`, Helm `appVersion` / image tags, `CHANGELOG.md` all agree.
2. [ ] CI green on `main`.
3. [ ] Local dry-run: `python -m build` in `python/`, `npm pack` in `typescript/runkite-runner/`, `goreleaser release --snapshot --clean` (optional).
4. [ ] Secrets present (see below) if you want PyPI/npm on this tag.
5. [ ] Tag and push:
   ```bash
   git checkout main && git pull
   git tag -a "v$(tr -d '[:space:]' < VERSION)" -m "v$(tr -d '[:space:]' < VERSION)"
   git push origin "v$(tr -d '[:space:]' < VERSION)"
   ```
6. [ ] Watch the **Release** workflow; verify GitHub Release assets, `docker pull`, `pip install`, `npm view`.
7. [ ] Confirm README + site install commands match the published version.

## One-time setup (humans)

### PyPI

1. Create an account on https://pypi.org (and verify email).
2. Account settings → API tokens → add token (scope: entire account or project `runkite-runner` after first upload).
3. GitHub → `getrunkite/runkite` → Settings → Secrets and variables → Actions → New repository secret:
   - Name: `PYPI_API_TOKEN`
   - Value: the token (including `pypi-` prefix).

First upload creates the project; later releases need the same token (or a project-scoped one).

### npm

**Preferred: Trusted Publishing (OIDC)** — no long-lived publish token.

1. Create the package once (first version only), from a clone with 2FA:
   ```bash
   cd typescript/runkite-runner
   npm publish --access public --otp=<authenticator-code>
   ```
2. On https://www.npmjs.com/package/runkite-runner → **Settings** → **Trusted Publisher**:
   - Provider: GitHub Actions
   - Organization: `getrunkite`
   - Repository: `runkite`
   - Workflow filename: `release.yml` (filename only)
   - Allowed action: `npm publish`
3. Later tags publish via OIDC in `.github/workflows/release.yml` (`id-token: write`).

**Fallback:** granular access token with **Bypass 2FA** checked → GitHub secret `NPM_TOKEN`. Classic tokens are revoked on npm; if CI returns `EOTP`, the token did not bypass 2FA — use Trusted Publishing or re-create the granular token with bypass enabled.

### GHCR

Uses `GITHUB_TOKEN`. After the first push, set package visibility to **Public** under GitHub → Packages (`runkite`, `runkite-runner`, `runkite-runner-ts`) so `docker pull` works without auth.

### Stale tag note

An old annotated tag `v1.0.0` may still exist from early development and does **not** match current `main`. Public install docs target **`v0.1.0`**. Do not move `v1.0.0` onto current main without an explicit decision.
