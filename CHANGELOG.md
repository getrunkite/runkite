# Changelog

All notable releases are documented here. Version source of truth: [`VERSION`](./VERSION).

## [Unreleased]

### Added
- Fixture replay: `runkite sim -f fixtures.yaml` creates runs with `X-Runkite-Simulation` (admin-gated). Spend holds, usage ingest, and UTC-day run caps skip; concurrent admission, rate limits, kill, and connector policy stay. Admin `sim` badge. YAML matcher for deny/pending audit rows (SQL). See [docs/sim.md](docs/sim.md).
- Opt-in connector MCP payload shrink (`payload_shrink` in `langgraph.json`, default off). Over-cap `tools/call` results stash in Redis; the runner sees a preview plus `runkite_retrieve_payload`. Not a token SLA; audit keeps the preview, not the original body.

## [0.4.1] — 2026-09-15

Security patch: connector HITL one-shot is bound to this `tools/call`'s argument digest. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- `pending_actions.args_digest` / `args` / `decided_by` (schema 14); consume and find match the digest
- Admin Pending **Args** column; approve records `decided_by` when Admin auth carries an identity

### Fixed
- Approving a $500 transfer no longer forwards a $5,000,000 retry on the same tool (consume used to match `(run_id, generation, connector, tool)` only)
- `google.golang.org/grpc` 1.83.1 → 1.83.2 (already on `main` before this tag)

### Notes
- No Python or TypeScript runner logic change — version lockstep only.
- Consume still skips Decide so mandatory HITL cannot re-pending the approved retry. Digest match *is* the retry check.
- Rows created before this binding (`args_digest` empty) still consume once on the old tuple.
- Display map omits secret-looking keys; approve re-eval uses that map. Predicates must not key on `password|token|secret|authorization|api_key`.
- Fixture replay (`runkite sim`) is not in this tag.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.4.1]: https://github.com/getrunkite/runkite/releases/tag/v0.4.1

## [0.4.0] — 2026-09-14

Argument-aware connector policy: MCP `tools/call` arguments reach Decide, config predicates, webhook, and audit. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- Connector MCP `tools/call` arguments bound into policy Decide (SHA-256 digest for cache / webhook / audit; size-capped, secret-stripped display map)
- Config `policy.predicates` — deny or pending from argument values after a matching grant and before the sync webhook (never allow; restart to reload; no Admin CRUD)
- Bounded expirable LRU for policy decisions (max 10_000); cache key includes `ArgsDigest`; `pending` is not cached
- Admin Audit **Args** column; Grants / Pending copy points operators at `langgraph.json` for predicates
- Operator how-to: [Grants & HITL](site/support/grants.html#argument-predicates)

### Changed
- Policy webhook `data` includes `args_digest` plus capped `args` on `tool.call`
- `tool_auth` and SIEM carry the digest only

### Notes
- Connector MCP, pre-call only — not in-graph `AuthorizeTool`, not tool-result inspection. Pending rows still store connector / tool / reason; the triggering arguments are on the audit event.
- Fixture replay (`runkite sim`) is not in this tag.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.4.0]: https://github.com/getrunkite/runkite/releases/tag/v0.4.0

## [0.3.0] — 2026-09-05

Self-host preview: FinOps, opaque adapter checkpoints, plane-side secrets/RLS/`allowed_tools`, Admin playground + Spend, docs rewrite. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- FinOps continuum on SQL: pricebook, daily USD/token/run caps, reservation holds, routing aliases, alerts/export, cancel-on-hard-breach, Admin Spend + live overlay editor
- Universal opaque checkpoints for non-LangGraph adapters (`adapter-state`); proxy HITL/CAS still for LangGraph
- Per-graph `allowed_tools`; connector `auth.secret_ref` (env/file/Vault); Postgres RLS fail-closed
- Run manifest snapshot at dispatch; Admin Try-agent playground, registry editor, DocsLink
- Kind K0–K3 ops proofs plus a named EKS smoke/reclaim
- Docs site rewrite, Admin UI 15-screen guide, Gemini dogfood QA hub
- Admin login rate limit (TCP peer, not `X-Forwarded-For`); GitHub Actions SHA-pinned

### Fixed
- Store `ListNamespaces` suffix on every backend; Python store `batch()` fail-fast on the store loop
- Proxy checkpoint lock map capped; RunDetail SSE list capped
- Alpine runtime image digest-pinned; Docker `GIT_COMMIT` is a release build-arg

### Notes
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.3.0]: https://github.com/getrunkite/runkite/releases/tag/v0.3.0

## [0.2.0] — 2026-08-10

Governance-preview cut: plane policy + Admin governance on SQL backends, announce-bar proof, packaging smokes. Preview (BUSL) — not a 1.0 production claim.

### Added
- Fail-closed connector policy (grants, sync webhook PDP, durable SQL audit)
- Connector HITL (`pending` → Admin approve → one-shot agent tool retry)
- Mandatory HITL overlays, kill/pause switches, time-bounded break-glass
- Run admission (agent-scoped authz, concurrent/daily `admission_limits`)
- Admin UI pages: grants, mandatory HITL, pending, kill, break-glass, audit
- `make smoke-governance` / CI announce-bar coverage on Postgres
- Kind Helm packaging smokes (`kind-helm-smoke` … `kind-helm-net`)
- MCP connector session tokens; Redis-shared Admin sessions; Helm pod TLS wiring
- Trust notes: [`docs/trust-governance.md`](docs/trust-governance.md)

### Fixed
- Policy decision cache key includes `principal` (no Alice→Bob allow leak when `cache_ttl_ms` > 0)

### Notes
- **Supported HA:** Postgres + Redis (Compose soak). Kubernetes/Helm remains **Compatible** (kind install/ops smokes; paid EKS deferred).
- Connector HITL approve is Admin-only; agent must retry the tool after approve (not Agent Protocol interrupt/resume).
- Mongo is not equal for durable governance (audit / grants / pending → `501` / fail-closed).
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.2.0]: https://github.com/getrunkite/runkite/releases/tag/v0.2.0

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
