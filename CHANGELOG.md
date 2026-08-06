# Changelog

All notable releases are documented here. Version source of truth: [`VERSION`](./VERSION).

## [0.1.1] — 2026-08-06

### Changed
- Richer PyPI / npm package READMEs and clearer root README links
- GitHub Release notes lead with download links (assets still listed by GitHub below)
- Helm default image tags use `:latest` for floating pulls

[0.1.1]: https://github.com/getrunkite/runkite/releases/tag/v0.1.1

## [0.1.0] — 2026-08-06

First public preview release of Runkite.

### Added
- Go control-plane binaries via GitHub Releases (linux/darwin, amd64/arm64)
- Container images on GHCR: `ghcr.io/getrunkite/runkite`, `runkite-runner`, `runkite-runner-ts`
- PyPI package `runkite-runner` (Python LangGraph runner)
- npm package `runkite-runner` (TypeScript / LangGraph.js runner)
- OpenAPI specs attached to the GitHub Release
- Helm chart defaults pointed at GHCR images

### Notes
- Preview quality: use the Supported profile (Postgres + Redis + auth) for anything serious.
- Honest gaps: [docs/limitations.md](./docs/limitations.md) release summary.
- Site: https://getrunkite.github.io/runkite/

[0.1.0]: https://github.com/getrunkite/runkite/releases/tag/v0.1.0
