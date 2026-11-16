# Current-source integration and qualification checklist

Status: Unreleased integration work after published v0.9.0. This document is a
maintainer checklist, not a release announcement, merge-status tracker or claim
that every listed path is already present in a partial feature checkout.
The public entry point is [Integrations](../integrations.md#unreleased-current-source).

## Assemble the matching source tree

The navigation change expects the following feature documents and support
sections to land with their implementations. A partial checkout can therefore
have real missing local links. Integrate the source dependencies and rerun the
normal checker; do not add exclusions, placeholder pages or fake successful
qualification receipts.

- Registry form preservation: `admin-ui/src/lib/registryEntryForm.ts` and
  `docs/registry.md`. Review reference: [PR #73](https://github.com/getrunkite/runkite/pull/73).
- Scoped authority and durable observation:
  `docs/internal/scoped-specialist-access.md`, `docs/auth.md`,
  `docs/mcp-server.md`, `site/support/security.html` and the updated
  `examples/trinity_specialist_mcp` caller. Review reference:
  [PR #74](https://github.com/getrunkite/runkite/pull/74).
- Typed nested MCP discovery: `internal/api/mcp_schema.go`,
  `docs/internal/mcp-input-schemas.md` and `site/support/protocol.html`.
  Review reference: [PR #75](https://github.com/getrunkite/runkite/pull/75).
- Opaque numeric preservation: `docs/internal/opaque-json-preservation.md`,
  `docs/registry.md`, the raw Admin metadata transport and SQL/Mongo regressions.
  Include `admin-ui/src/lib/registryEntryRoutes.ts` and exact Agent/Registry
  tenant selectors. Keep existing-name editing separate from creation mode,
  including the literal registry name `new`, and retain stale-form save guards.
  Review reference: [PR #79](https://github.com/getrunkite/runkite/pull/79).
- Capability descriptors and catalog requirements:
  `docs/internal/agent-capabilities.md`, `site/support/agents.html` and
  `site/support/registry.html#requirements`.
  Review reference: [PR #83](https://github.com/getrunkite/runkite/pull/83).
- Retained cron visibility: `docs/internal/cron-run-visibility.md` and
  `site/support/cron.html`, with exact-tenant Thread/Run navigation. Retained-run
  links open guarded, read-only snapshots with manual Refresh, without live
  SSE, cancellation or child-run navigation.
  Review reference: [PR #82](https://github.com/getrunkite/runkite/pull/82).
- Offline policy fixtures: `docs/policy-fixtures.md`,
  `docs/internal/offline-policy-fixtures.md`, `examples/policy_fixtures` and
  `site/support/policy.html#offline-regression-fixtures`.
  Review reference: [PR #76](https://github.com/getrunkite/runkite/pull/76).
- Exact policy identities: `docs/internal/exact-policy-identities.md`,
  `docs/trust-governance.md`, `docs/configuration.md` and `docs/ops-runbook.md`.
  Preserve exact positive identities, conservative restrictive matching and the
  upgrade/review procedure. The decision cache's exact tuple includes the
  captured policy revision; old in-flight evaluations cannot repopulate authority
  for a reloaded revision. Include `admin-ui/src/lib/policyGrantForm.ts` and
  `admin-ui/src/pages/PolicyGrants.tsx`: loaded row/scope bytes and list filters
  stay exact, and tool fields use validated JSON string arrays.
  Review reference: [PR #78](https://github.com/getrunkite/runkite/pull/78).
- Native Langflow caller: `examples/langflow_mcp`,
  `docs/internal/langflow-mcp-boundary.md`, `site/support/langflow.html` and
  `.github/workflows/langflow-mcp.yml`. The source fixture uses only its agent
  run grant for `/mcp`; separate REST observation authority is unnecessary.
  Review reference: [PR #81](https://github.com/getrunkite/runkite/pull/81).
- Native promptfoo provider: `examples/promptfoo`,
  `internal/api/promptfoo_qualification_test.go`,
  `docs/internal/promptfoo-managed-provider.md`, `site/support/promptfoo.html`
  and `.github/workflows/promptfoo-example.yml`. Its Node-only directory's
  minimal `go.mod` keeps upstream Go templates out of root Go discovery;
  first-party Go qualification stays in `internal/api`.
  Review reference: [PR #80](https://github.com/getrunkite/runkite/pull/80).
- AG-UI text delivery: `examples/managed_ag_ui`, the additive shared chunk
  helpers in `examples/managed_ai_sdk/bridge.ts`,
  `docs/internal/managed-ag-ui-bridge.md`,
  `site/support/sdk-integration.html#managed-ag-ui` and
  `.github/workflows/ag-ui-example.yml`.
  Review reference: [PR #77](https://github.com/getrunkite/runkite/pull/77).

These public PR links identify review context only. The maintainer records
actual merge, hosted CI and publication status separately. Do not invent PRs
for slices that do not yet have one. A commit on `main` is still Unreleased
until coordinated publication lists it.

When combining guide edits, keep both registry precision/preservation and
declared requirements, including `#metadata-preservation` and `#requirements`.
Keep both `#offline-regression-fixtures` and `#exact-policy-identities` on the
policy page. Preserve `#structured-input-discovery` on the protocol note,
`#agent-authority` on the security guide and `#managed-ag-ui` on the SDK guide.
The real `langflow.html` and `promptfoo.html` guides must be present before the
catalog/navigation change is deployed. Copying a navigation page alone does
not satisfy these dependencies.

## Qualify the integrated contracts

1. **Keep identity and payload boundaries.** Run exact identity, current caller,
   retained retry, run-reference and deletion/recreation regressions. Verify
   static policy restrictions still deny padded aliases before positive/default
   authorization. Opaque registry/capability data must survive real SQL reads,
   bootstrap and the actual Admin request builder without a definition-version
   churn on unchanged restart. Reconcile the capability and precision patches
   together; do not infer that independent tests cover their read/merge/write seam.
2. **Rebuild actual Admin.** Use its declared Node/npm toolchain; run the focused
   UI tests, lint, typecheck and build. Verify Agent Detail/Try Agent unknown
   states, read-only registry declarations, stale/empty/unavailable cron states,
   exact tenant links and replacement-run rejection.
   Verify retained-run snapshots expose manual guarded Refresh but no live SSE,
   cancellation or child-run navigation; no stream incarnation fence is claimed.
   Exercise Agent/Registry selection changes, explicit empty-default versus
   omitted-global selectors, the literal registry name `new`, and form
   remount/save/delete guards. Grant
   form roundtrips must retain exact row/scope bytes and comma/space tool names;
   invalid JSON tool lists must fail before a write. Generated Admin assets are
   owned by the feature integration, not this navigation pass.
3. **Keep discovery advisory.** Exercise low-level MCP calls whose raw input
   disagrees with advertised defaults/types, local reference relocation, bounded
   generic fallback, unknown freshness and existing-session snapshots. Reconnect
   callers after authority or catalog changes.
4. **Keep logical fixtures offline.** Run `go test ./cmd -run '^TestOfflinePolicy'` and
   the real `policy test` example/exit-status checks from `docs/policy-fixtures.md`.
   No config/backend/secret/model client may be introduced by fixture loading.
5. **Use native caller gates.** Run the dedicated Langflow, promptfoo and AG-UI
   workflows with their pinned dependencies, separate required environments and
   existing bounded supervisors. Langflow's baseline read/write receipt is not
   scoped-source proof; run `examples/langflow_mcp/qualify.py --scoped-source`
   separately against the integrated scoped implementation. That native caller
   profile tests run-only MCP authority, not REST observation references; retain
   the separate scoped server/API reference tests. The AG-UI proof must retain
   both shared-bridge regressions and real runner detach/reattach/Stop cases.
   The promptfoo gate must exercise the native scheduler retry guard and fresh
   intentional repeats, then the actual public API with its scripted worker.
6. **Preserve evidence limits.** Save only nonsecret synthetic receipts through
   each workflow's allowlisted artifact path. Check exact tested source and
   dependency versions, skipped cases, stores/transports and asserted counts.
   Native headless client tests do not qualify a visual editor or provider loop;
   scripted model/worker tests do not establish production provider behavior.
7. **Check tooling and dependencies.** Run root `go list ./...` after installing
   the Node examples, affected first-party tests and linters, and the examples'
   locked dependency audits. Confirm that module boundaries isolate upstream
   templates while first-party fixtures remain in the root test suite. Fix a
   finding or identify the exact blocked source without check exclusions or an
   unrelated forced package upgrade.
8. **Check discoverability and availability.** Run
   `python3 scripts/check-doc-links.py` and `make test-site` on the assembled
   tree. Verify Markdown destinations, support catalog/feature-map/engineering
   navigation and all new anchors. Keep the v0.9.0 release badge, package/install
   labels and existing animations. A later release updates versions, artifact
   contents and labels together in its own authorized change.
   Run `python3 scripts/openapi/check.py`. A route-contract check is not native
   caller qualification.

## Recorded local integrated qualification

The November 16, 2026 integrated API contract check covered 169 Go routes, with
spec integrity checks passing. This count records that source snapshot.

On November 16, 2026, Langflow's baseline read/write and run-only scoped profiles
both passed against integrated local source. Each retained exactly two managed
runs and two real Python worker assignments, without redispatch on reconnect
or failed-run retry. Both used LFX 1.13.2, MCP 1.30.0, LangChain Core 1.8.1 and
Pydantic 2.13.5 with MCP protocol `2025-11-25`. The separate synthetic receipts
are `outputs/runkite-next/research-20261116/langflow-integrated-baseline.json`
and `outputs/runkite-next/research-20261116/langflow-integrated-scoped.json`.
These receipts qualify the native headless MCP client, not its visual editor,
browser credential handling, model loop, REST observation or production proxy.

The pinned promptfoo 0.125.1 ordinary evaluator also passed separately with
broad tenant read/write and scoped run/observe authority on Node 26.11.0. Each
profile produced four distinct runs and threads and four reference-pinned waits;
the scoped profile additionally passed all six denial probes. The receipts are
`outputs/runkite-next/research-20261116/promptfoo-final-baseline.json` and
`outputs/runkite-next/research-20261116/promptfoo-final-scoped.json`. Both used
SQLite, in-process transport and a scripted worker with zero model calls. They
do not qualify real providers, arbitrary evaluator strategies, upstream UI/server
features, Postgres/Redis or multi-replica behavior. These local results do not
assert a hosted workflow pass or publication, and must be rerun after relevant
source or dependency changes.

On November 17, 2026, the separate [Langfuse OTLP qualification](langfuse-otlp.md)
passed against unmodified local Langfuse v4.81.0. Actual Go/Python/TypeScript
exporters and production run-span/LangChain callback helpers produced seven
persisted ordinary spans across three services, with exact trace/parent
identity and operational metadata. Both missing/invalid-auth API read probes
were denied. No generation, model call or business tool execution was involved.
The owned backend was removed after qualification. See the
[qualifier commands](../../examples/langfuse_otlp/README.md) and
[sanitized receipt](../../examples/langfuse_otlp/qualification.json).
This result does not qualify plane dispatch, framework workers, backend UI,
real providers or HA. The wire/offline workflow excludes the native backend;
the earlier native caller results do not expand this helper-boundary proof.

## Keep demand-led work deferred

The language-qualified [Langfuse OTLP recipe](../langfuse.md) and its own
[exporter and receiver gates](langfuse-otlp.md) passed the scoped local checks
recorded above. Production rollout and any additional GenAI attribute mapping
still need caller-specific qualification.

Temporal and Cloudflare caller recipes, effective OpenShell policy
qualification, response inspection,
tools-only connector profiles, per-fire cron receipts and requester-private
thread ownership remain separate designs or demand-led work. No catalog
installer, workflow runtime, policy engine, vault, identity issuer, observability
backend or frontend framework is embedded by this program. Native tools that
bypass an existing gate do not acquire connector governance from a descriptor
or integration receipt. Qualification does not imply upstream endorsement.
