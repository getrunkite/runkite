# A local proposal, human review and one note

Availability: checkout example introduced with v0.9.0 source and retained in
v0.10.0. The governance
decision API already exists, and the independent helpers were first published
in v0.8.0. This scripted demo mode is an example asset, not a registry package.

This walkthrough keeps the existing application and its real AI SDK tool path.
It replaces only the model with an explicit local script. The script always
proposes the same harmless note, regardless of the prompt:

```json
{
  "title": "Release plan",
  "body": "Review the changes before publishing."
}
```

No model-provider account or key is needed. Runkite still makes the real policy
decision, the reviewer still approves in the tenant workspace, and the app still
stores and continues the operation. This is a governance demonstration, not an
LLM quality test or a managed-run demonstration.

## 1. Prepare an isolated plane and three roles

Use a fresh local SQL database and a self-hosted Runkite plane. Postgres, MySQL
or SQLite can store approvals and audit. Do not run this exercise against
production data or change a live tenant's grants to demonstrate a deny.

The application server binds to `127.0.0.1`. The Go plane's current CLI listeners
bind all interfaces, so keep that plane on your existing isolated development
host/network. Do not publish its HTTP or gRPC ports for this demo. This guide
does not change a firewall, expose a service or start a hosted backend.

[demo-policy.json](demo-policy.json) is a **merge template**, not a standalone
`langgraph.json`. It supplies these identities and the exact policy tuple:

- App: `notes-backend`, tenant `default`, permission `governance:decide`.
- Reviewer: `notes-reviewer`, tenant `default`, permission `governance:approve`.
- Operator: a separate global Admin credential for Grants, Pending and Audit.
- Tool: `notes-app` / `workspace` / `createNote`, granted and held for review.

Replace the three literal `replace-with-*` map keys through your normal local
credential setup. They are placeholders, not credentials to deploy. Copy the
new key entries into `auth.keys` and the named rules into the corresponding
policy arrays. Preserve all existing keys, unrelated grants, runner
authentication and operator recovery access. Do not replace a JWT/webhook auth
provider with the template; map the same roles through that provider instead.
The template requires audit persistence and denies unmatched tools.

Preserve the full configuration's `graphs` section and storage settings. The
loader still requires a graph even though this app never dispatches it. For a
fresh echo-based development configuration, the existing graph mapping is:

```json
{
  "graphs": { "echo_agent": "./graph.py:graph" },
  "dependencies": ["."]
}
```

That mapping comes from `examples/echo_agent/langgraph.json`. Keep its normal
path context when merging. Do not register `notes-app` as a managed agent or
define an MCP connector called `workspace`; those names are logical policy
identifiers on this decision-only path. No runner process is required.

Restart the isolated plane after changing its file configuration. On an already
enabled policy engine, the same grants and HITL rules can be added through the
existing Admin overlays. Use the [operator setup guide](../../docs/app-owned-governance.md)
for the complete authentication, SQL and review prerequisites.

For a new SQLite development instance, choose an unused database path and the
full merged configuration path. Run from a clean development shell with no
external database, queue or telemetry environment configured:

```sh
: "${DEMO_CONFIG:?Set the full merged demo configuration path first}"
: "${DEMO_DATABASE_PATH:?Set an unused local SQLite database path first}"
DATABASE_PATH="$DEMO_DATABASE_PATH" runkite dev --config "$DEMO_CONFIG"
```

`POSTGRES_DSN`, `MYSQL_DSN` or `MONGO_URI` would select another backend instead of
that SQLite file. Stop and correct the isolated environment if startup reports a
different backend. The main example README's installation instructions build
the helper; install or build the Go binary separately using the normal project
quickstart. The next steps keep this plane running in its own terminal.

## 2. Build the checkout helper and start the scripted app

Use Node.js 22.13 or newer. From the repository root:

```sh
cd typescript/runkite-governance
npm ci
npm run build
cd ../../examples/governance_ai_sdk
npm ci
```

This example depends on the checkout helper through its existing file
dependency. A registry application can instead install `runkite-governance`.
The scripted mode uses `MockLanguageModelV4` from the already pinned `ai/test`
export. It adds no dependency and does not configure a second policy engine.

In the app server's shell, set `RUNKITE_HTTP_URL` to your isolated plane,
`RUNKITE_API_KEY` to the **app** credential, and `APP_TOKEN` to a separate local
application credential. Keep operator/reviewer credentials out of this process.
For a plane on the default local port:

```sh
export RUNKITE_HTTP_URL=http://127.0.0.1:2026
: "${RUNKITE_API_KEY:?Set the isolated app governance credential first}"
: "${APP_TOKEN:?Set the local application credential first}"
export APP_STATE_FILE="$PWD/data/scripted-demo-state.json"
RUNKITE_DEMO_MODEL=scripted npm start
```

Use a previously unused state-file path for a fresh exercise. If that file
already contains notes, preserve it and choose another path. A single app process
owns this example file. Do not run multiple app processes against the same file.

Expected startup includes:

```text
Governance AI SDK example listening on loopback port 8088
Scripted local demo. No model provider is called. Each new turn proposes: ...
```

The response also identifies `modelMode: "scripted"`. This switch is server
configuration only; a browser request cannot select a different model mode.
Without it, the normal provider path remains the default and requires
`OPENAI_API_KEY` and `MODEL`. A missing provider key never silently enables a mock.

## 3. Submit a proposal and inspect the hold

Use a separate app-client shell with the same local `APP_TOKEN`. Set
`RUNKITE_HTTP_URL` to the same isolated plane URL in any shell where a later
command uses it; shell variables are not shared between terminals.

```sh
curl -i http://127.0.0.1:8088/turns \
  -H "Authorization: Bearer $APP_TOKEN" \
  -H 'Idempotency-Key: demo-note-1' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Propose the scripted release-plan note."}'
curl -s http://127.0.0.1:8088/notes \
  -H "Authorization: Bearer $APP_TOKEN"
```

With the matching grant plus HITL rule, expect HTTP `202`, `modelMode: "scripted"`,
`toolErrors: 0` and one job with `status: "pending"`. `/notes` returns `{}` for the
fresh app store. The returned job contains the fixed arguments, an application
`id` and a Runkite `actionId`. Save both values from the actual response:

```sh
export JOB_ID=copy-the-64-character-job-id-from-the-response
export ACTION_ID=copy-the-actionId-from-the-response
```

Those placeholders describe response fields; do not submit them literally.
The job ID is also the submitted governance `run_id`, an operation correlation
value. It is not a managed run ID. The action ID identifies the pending review.

Before approval, continuation remains held:

```sh
curl -i -X POST "http://127.0.0.1:8088/actions/$JOB_ID/continue" \
  -H "Authorization: Bearer $APP_TOKEN"
```

Expect HTTP `202`, `status: "pending"` and no note. The continuation accepts no
replacement arguments or browser approval flag.

## 4. Approve as the tenant reviewer

Open `$RUNKITE_HTTP_URL/admin/review/` in a browser and sign in using the
**reviewer** credential, not the app key or the global operator key. Confirm
tenant `default` and the displayed reviewer identity. In **Approvals**, find the
returned `ACTION_ID`, inspect the tool and original note arguments, and approve.

The default single-review policy reaches approved after that vote. If your
isolated configuration deliberately requires a quorum, it remains pending until
enough distinct verified reviewers approve. Changing the approval plan requires
a fresh decision. Do not relax a real policy to make a demo finish.

GET `/notes` again. It is still `{}`. Approval records permission; it does not
call the app, run its tool or resume its model loop.

## 5. Continue once and see one note

Repeat the application's continuation request, then read its notes:

```sh
curl -s -X POST "http://127.0.0.1:8088/actions/$JOB_ID/continue" \
  -H "Authorization: Bearer $APP_TOKEN"
curl -s http://127.0.0.1:8088/notes \
  -H "Authorization: Bearer $APP_TOKEN"
```

Expect a job with `status: "done"` and `result.noteId` equal to `JOB_ID`. Notes
now contains one entry keyed by that ID with the fixed title and body. Repeat
the continuation: it returns the same note ID and creates no second note.
Restarting this app with the same state file preserves the result.

Repeating the original `/turns` request with the same idempotency key and prompt
returns the recorded turn/jobs without another proposal. Reusing that key with a
different prompt is an error. Each genuinely new turn needs a different key.

## 6. Follow the same operation in Admin

Use the separate **operator** credential in global Admin. In **Pending**, include
approved actions in the status filter to find the same `ACTION_ID`. In **Audit**,
filter by the operation `run_id`, which is
`JOB_ID`, to inspect the correlated tool decision. The initial policy decision
remains a pending decision in audit; approval changes the action record and does
not rewrite history to claim that a tool executed.

If desired, in a separate operator shell with its own `RUNKITE_OPERATOR_KEY`:

```sh
curl -s --get "$RUNKITE_HTTP_URL/admin-api/audit-events" \
  --data-urlencode "run_id=$JOB_ID" \
  -H "Authorization: Bearer $RUNKITE_OPERATOR_KEY"
```

Do not add that operator key to the app process. Review's tenant-scoped role
does not grant global Audit or policy editing.

This app creates no Runkite Thread or Run. On a fresh isolated plane, those
lists stay empty throughout the exercise. Other pre-existing managed work may
still be visible on a shared development plane. Spend, managed run manifests
and managed connector-action evidence do not gain entries for this operation.
The app's stored note is the execution result. Audit records the decision.

## 7. Try rejection, policy deny and a plane outage

- **Human rejection:** submit a new turn with `Idempotency-Key: demo-note-reject`.
  Deny its new action in tenant Review, then continue its saved job. The job
  becomes `denied` and no new note appears. Previously created notes stay intact.
- **Policy deny:** only in the isolated scope, remove the matching `notes-create`
  effective grant, including any Admin overlay, while retaining
  `default_effect: deny`, restart after a file edit and
  submit a new turn with `Idempotency-Key: demo-note-deny`. Expect one `denied`
  job, `toolErrors: 0`, no new note and no new approval row. A HITL rule cannot
  turn a deny into permission. Restore the isolated grant before the next test.
- **Plane outage:** stop only your own isolated plane and submit a new turn with
  `Idempotency-Key: demo-note-outage`. The SDK reports a tool error; this small
  example returns `toolErrors: 1`, no job and no new note. Its HTTP `200` only
  means the SDK request completed, not that a tool succeeded. There is no
  fallback execution or provider call. The failed turn stays recorded and is
  not automatically replayed after the plane returns. Use a new application
  operation key only when intentionally retrying a fresh proposal.

Do not infer business success from HTTP status, model text or an allow decision.
Check the application's persisted job and note. An outage after the plane saved
a pending row can leave an orphaned approval with no saved app job; approval
alone still does not execute anything. Recover that case deliberately, without
automatically repeating an uncertain external write.

## Checks and limits

From this example directory, `npm test` runs the existing state, guarded-tool and
streaming suites plus scripted-mode checks. `npm run check` validates the pinned
SDK types. The automated unit tests use a fake policy transport. Following this
walkthrough against your real plane supplies the real approval/audit exercise.
No model provider is called in either scripted path.

The local app store demonstrates single-process idempotency for harmless local
notes. A deployed app needs its own database transaction and, for external
effects, downstream idempotency or its existing outbox. Polling an approved
action does not consume approval. Pending proposals expire after 24 hours; that
is not an execution deadline for an already-approved operation.

This path supplies no managed run kill switch, break-glass, spend cap, sandbox,
secret injection or enforcement of calls that skip Decide. For those managed
records and controls, select the documented runner and connector paths instead.
