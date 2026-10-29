# Runkite Admin UI

The operational dashboard for a self-hosted Runkite plane. React, TypeScript and
Vite build static assets directly into `../internal/adminui/dist/`. Go embeds
those committed assets and serves the app at `/admin/`; end users do not need
Node.js or a separate frontend deployment.

## Develop

Use Node.js 22 or newer and npm 10.9.2, matching `package.json` and CI. Keep the
lockfile in npm 10 format. From this directory:

```sh
npm ci
npm run dev
```

Open `http://localhost:5173/admin/`. Vite proxies `/admin-api/*` to the plane at
`http://localhost:2026` without rewriting the prefix. Start that plane separately
with your intended configuration and runners. In local development,
`runkite dev --config examples/all_agents/langgraph.json` provides demo agents.

The production shell is public, while `/admin-api/*` requires the configured
Admin credential. The login flow uses an HTTP-only session cookie; mutations
include CSRF protection. Pure local mode without configured auth skips login.
Do not persist Admin credentials in browser storage. Reuse the existing session
and CSRF client for new API calls.

## Verify and refresh the embedded bundle

```sh
npm run lint
npm test
npm run build
```

`npm run build` runs TypeScript checking and Vite, then replaces
`internal/adminui/dist/` directly. There is no copy step. Include the changed
source and rebuilt embedded files together when shipping UI changes; otherwise
the Go binary still serves the previous bundle. `go build ./cmd/` consumes the
embedded files without running npm. CI performs the install, lint, tests and
build before compiling Go.

Native Node tests cover message normalization, tenant-scoped API paths,
durable-run reconciliation and tool-authorization hop state. Copy-only edits normally need the
existing checks, not new snapshots of the same text. To inspect a production
bundle locally, use `npm run preview` or serve the rebuilt Go plane.

## Operator semantics

- Threads, Runs and Try agent operate on runner-managed executions. A standalone
  `/governance/decide` call creates neither a thread nor a run.
- Pending and Audit also show in-process decisions. Their `run_id` can be an
  app-owned operation ID, so it is not always a managed-run link. Approval records
  an operator decision; the connector caller retries or the app continues the
  exact saved operation. Admin does not execute the app's tool.
- Kill switches, break-glass and managed-run spend controls do not apply to the
  standalone decision API. Estimated USD comes from recorded tokens and the
  configured pricebook or a reported gateway cost. Cancellation can finish before
  trailing usage arrives, and a cancel cannot undo an external write.
- Runner capability controls native interrupt/resume. The unreleased managed
  AI SDK profile creates real managed records but does not support native approval
  resume. The app-owned governance helpers keep continuation in the app.

See the [Admin operator guide](../docs/admin.md),
[integration guide](../site/support/sdk-integration.html), and
[source capability limits](../docs/limitations.md). Shared support URLs live in
`src/components/common.tsx`; page links should use `supportPage(...)`.
