# Advisory structured input discovery over MCP

Status: implemented in v0.10.0. Published v0.9.0 keeps the generic MCP
input schema. This change enriches discovery without changing execution,
permissions, retry identity, storage migrations or the Runner Protocol.

## Contract and ownership

Each agent still exposes the existing `message`, `input`, `thread_id` and
`run_id` envelope. The control plane can replace only the nested `input`
property's generic object schema with a usable stored `AgentSchema.InputSchema`.
The schema describes the raw agent input, not the whole tool envelope.

Python static LangGraph graphs already report schemas after load through
`handleReportAgentSchema`. Other adapters, factories and failed/missing reports
may retain stubs. A separately supplied report is not evidence that a particular
worker has loaded matching code. `AgentSchema` still has no reporter revision,
report time or deployment binding. This feature does not invent that provenance.

The v0.10.0 [static graph publication recovery](schema-publication-recovery.md)
caches Python static reports once and uses a separate periodic endpoint to fill
only existing exact four-field bootstrap rows after a reset or failed report.
It does not replace useful/partial reports, introspect factories or other SDKs,
or attest which same-kind worker's code is current. Ordinary startup PUT retains
its replacement behavior. Publication recovery and MCP catalog refresh are
different operations; the session and unknown-freshness rules below still apply.

The tool's `_meta["getrunkite.com/input-schema"]` states:

- `status` is `reported` or `generic`.
- `freshness` is always `unknown`, and `session_snapshot` is always `true`.
- A reported schema has `source: "agent_schema"` and a
  `reported_schema_sha256` digest of the original JSON-marshaled stored input
  schema. This identifies schema content, not deployed code or a reporter.
- A generic schema has a coarse `reason`, such as a missing report, a stub,
  unsupported schema structure, invalid references or a size/catalog limit.
  Internal storage errors and remote response bodies are not exposed.

`newMCPServer` reads the tenant-scoped reports when constructing the tool catalog.
An existing stateful session retains that snapshot after reports change. Open a
new session to discover updates. There is no schema subscription, freshness
claim or automatic replacement inside an existing catalog.

Schema descriptions, defaults, examples and enum values can contain sensitive
text even though a report is not runtime input or a conversation. Treat those
definitions as discoverable data, keep secrets/customer samples out of their
annotations and redact support captures. Publication logging does not turn
application/framework diagnostics into a general-purpose redactor.

## Supported schema subset and fallback

The input root must explicitly declare `type: "object"` and include meaningful
structural constraints. A bare object or description-only stub remains generic.
The supported dialect is JSON Schema draft 2020-12, declared or implicit.

The implementation deep-copies the report through bounded JSON encoding, moves
it under `properties.input`, and relocates local JSON Pointer `$ref` values from
`#` or `#/...` to that new position. This includes references through `$defs`,
escaped pointer segments and ordinary recursive object shapes. Only
schema-valued keywords are traversed. A `$ref` inside `default`, `const`, `enum`,
`examples` or an unknown extension remains instance/annotation data.

Schemas with resource identifiers, anchors, dynamic/recursive-reference
keywords, external references, other dialects or legacy `dependencies` fall
back to the generic envelope. Schema-valued legacy dependencies are refused
rather than risk binding a missed local reference to the surrounding tool
envelope. Malformed schemas, unresolved pointers and invalid patterns also
fall back. `jsonschema-go` checks the candidate structure and reference graph
with no remote loader. It performs no network fetches, default application or
invocation validation.

The entire reported JSON tree, including annotations, has a depth limit of 32,
a 4,096-value traversal limit and conservative byte accounting. Both the encoded
report and the enriched envelope must fit 64 KiB. One constructed catalog has
a 1 MiB budget for accepted enriched envelopes; generic envelopes, tool names,
descriptions and metadata are outside that budget. This is not a bound on the
entire MCP response. Exhausted budget leaves subsequent candidates generic.

Every fallback creates a fresh generic envelope, including failures discovered
after embedding, resolution or catalog charging. Neither the report nor the
shared generic schema is mutated.

## Execution stays unchanged

The low-level MCP SDK `Server.AddTool` path remains in use. The typed convenience
wrapper would add validation/default behavior, so it is deliberately avoided.
The handler forwards the original raw `input`; it does not fill defaults,
coerce values or reject arguments because they disagree with discovery. If
both `input` and `message` are present, `input` still wins. A message-only call
keeps the existing user-message wrapping.

Current caller authority, configured agent checks and run retry rules remain
independent. The reported shape grants no permissions, does not attest code and
does not make an unsupported runtime operation supported. Retrying a retained
`run_id` continues to observe the same stored intent; conflicting input remains
a tool-level error. Schema changes do not rewrite that intent or dispatch it
again.

## Verification

Focused tests exercise the actual builder and in-memory MCP SDK transport with
the real SQLite retry fixture. They cover reference relocation and annotation
non-rewriting, recursive and escaped references, source/output independence,
fresh generic fallback after late failures, bounds, unavailable reports,
tenant isolation, session snapshots and generic peers.

A low-level protocol call deliberately omits a required/defaulted property,
sends a string where the advertised type is integer, and includes an exact
large integer. Its actual queued assignment retains the raw input without
defaults or coercion. Completion, retained retry and conflicting retry then
exercise the existing managed-run behavior. These tests use no model, external
MCP server or paid service. They do not establish native integration with every
MCP client or every schema generator.
