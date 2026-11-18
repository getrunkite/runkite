# Agent Marketplace / Registry

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

A searchable catalog of agent definitions -- publish, discover, and deploy agent definitions. **Minimal viable registry** scope, chosen deliberately: publish/search/get/version-history via API and Admin UI, no security review workflow, and no automatic clone-and-execute deploy pipeline. A registry entry is metadata plus a `source_ref` -- a git URL, a plain URL, or an inline `langgraph.json` snippet -- pointing at where a human (or their own tooling) goes to actually wire it into a deployment. This is deliberately a catalog, not a package manager's install step: running arbitrary fetched code is a fundamentally different trust/sandboxing problem than a searchable listing, and out of scope here.

```
PUT    /registry/entries/{name}                   Publish (create or new version)
GET    /registry/entries/{name}                   Get current entry
DELETE /registry/entries/{name}                   Unpublish
POST   /registry/search                           Search (name substring, tags -- must have ALL, author -- exact)
GET    /registry/entries/{name}/versions          Version history, newest first
GET    /registry/entries/{name}/versions/{v}      One specific historical snapshot
```

```json
{
  "display_name": "Sales Qualifier",
  "description": "Qualifies inbound leads using firmographic data",
  "author": "alice",
  "tags": ["sales", "lead-gen"],
  "source_type": "git",
  "source_ref": "https://github.com/example/sales-qualifier@main"
}
```

Versioning follows the exact same convention as agent versioning above: publishing unchanged content doesn't bump the version, an actual content change does, and every bump writes an immutable snapshot to its own history (append-only, `git revert` not `git reset` -- see the versioning section above for the full rationale, identical here). A registry is private to the tenant that published it ("an internal/private registry for one's own agents," not a shared public catalog across tenants), same isolation convention as agents/threads/runs.

**Publishing from the Admin UI, not just the API**: `/admin/registry` is not read-only. `PUT`/`DELETE /admin-api/registry/{name}` (optionally `?tenant_id=`, default `"default"`) let an operator publish a new entry, edit an existing one, or unpublish it from a plain form -- no request body to hand-write. This is the exact same underlying `PublishRegistryEntry`/`DeleteRegistryEntry` store call the client-facing `/registry/entries/{name}` API above uses, just reached through the Admin session instead of a tenant API key, and every write is recorded to `audit_events` (`registry.entry.put` / `registry.entry.delete`). The "no deploy automation" limitation below applies identically regardless of which path created the entry.

Admin edits preserve the entry's loaded `metadata` object, including keys the
form does not display. Use the API to edit metadata. Saves still replace the
entry with the submitted values; this does not add concurrent-edit protection.

v0.10.0 carries loaded Admin metadata through a separate
`metadata_json` string so the browser does not round its numbers before a save.
Upgrade every HTTP replica and reload Admin before editing. A loaded entry from
an older replica without that field fails closed. SQLite and Postgres preserve
the supported stored decimal values; MySQL native JSON and Mongo HTTP-ingress
precision limits remain. Previously rounded values cannot be recovered. See the
[numeric transport and storage contract](internal/opaque-json-preservation.md).

**Known limitations, stated plainly**:
- **No security review workflow.** Anything published is immediately visible to every caller in that tenant -- there's no approve/reject step, unlike a real package registry's review queue.
- **No deploy automation.** Publishing an entry doesn't register a runnable agent -- `source_ref` still requires a human (or separate tooling) to actually wire the code into a `langgraph.json` and restart/reload the relevant runner. The registry is a catalog, not a deployment pipeline.
- **Admin lookups by name alone are ambiguous under a cross-tenant name collision.** `registry_entries`' real key is `(tenant_id, name)`, not `name` -- if two tenants both publish "sales-bot", `GET /admin-api/registry/{name}` (system context, no tenant filter) returns an arbitrary match, and `GET /admin-api/registry/{name}/versions` genuinely merges both tenants' histories into one list. An explicit `?tenant_id=` query param on both routes disambiguates by scoping to one tenant; the admin version response also exposes `tenant_id` per entry so a merged (no-param) response is at least distinguishable after the fact. Client-facing routes are unaffected (already tenant-scoped from the caller's own auth context, same as every other resource).

`PublishRegistryEntry` and `DeleteRegistryEntry` run inside a real Mongo transaction on the MongoDB backend (same as agent versioning's `UpsertAgent` -- see State Backends below), not the non-atomic entry-table-update-then-version-insert this section used to describe as a limitation. Requires Mongo to run as a replica set (`docker-compose.test.yml`/CI now do); a standalone `mongod` rejects the transaction outright rather than silently degrading.

<a id="optional-definition-requirements-unreleased-convention"></a>

## Optional definition requirements (v0.10.0 convention)

A catalog publisher can describe requirements in the existing opaque
`metadata.runkite_definition` object. This is a versioned documentation
convention, not a deployment manifest or a validated compatibility contract.
Published v0.9.0 already stores opaque metadata; the read-only Admin metadata
view and the preservation fix are available in v0.10.0.

```json
{
  "metadata": {
    "runkite_definition": {
      "schema_version": 1,
      "framework_kind": "python-langgraph",
      "runkite_version": ">=0.10.0",
      "packages": {"python": ["runkite-runner==0.10.0"], "node": []},
      "connectors": ["ticketing"],
      "sample_input": {"messages": [{"role": "user", "content": "Summarize ticket DEMO-1"}]}
    }
  }
}
```

`framework_kind` names the intended profile, not the execution-pool dispatch
name. `runkite_version` and package strings are human-reviewed requirements;
Runkite does not parse ranges, resolve packages or confirm installed versions.
List every actual framework dependency from the publisher's own tested lockfile;
the example names only the Runkite runner and is not a complete environment.
Connector names describe required operator configuration, not grants or secrets.
`sample_input` is optional nonsecret JSON for a human to inspect and adapt, not
an automatic test, execution request or proof of authorization.

Keep unknown metadata keys and unsupported convention versions intact. Admin
shows the loaded metadata as escaped JSON, with no executable links, commands,
installation action or sample-run button. Omit the convention when requirements
are unknown rather than infer them from a URL or framework label. Nothing reads
`source_ref`, a package URL or a secret reference while displaying this record.

Never put credentials, private conversation data or live customer inputs in
catalog metadata or samples. Ordinary tenant catalog visibility and version
retention apply. Existing replacement-PUT and concurrent-edit semantics remain;
read the current entry and preserve its other fields when publishing a new
version. Registry metadata and the configured agent capability descriptor are
independent declarations, with no automatic deployment or authority between them.
