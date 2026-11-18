# Offline static policy fixtures

Status: implemented in v0.10.0; example fixture files remain checkout assets. Published v0.9.0 has live simulation
but does not include `runkite policy test`.

## Caller and boundary

An administrator needs to check a proposed static grant, argument predicate or
mandatory-review change without dispatching an agent or executing an allowed
tool. `runkite sim` intentionally creates real runs and can call live connectors,
so silently turning it into an offline mode would confuse two different proofs.
The new command is separate and leaves live simulation unchanged.

This is one bounded CLI over the existing policy engine, not a second PDP,
snapshot service, policy installer or approval system. No upstream runtime,
model-assisted annotator, waiver logic or policy execution engine is imported.
The operator owns an explicit fixture snapshot and expected results.

## Implementation

`cmd/policy_test_fixture.go` defines a strict versioned JSON input adapter for
`policy.Config` and `policy.PolicyInput`. It accepts only static grants,
argument predicates, mandatory HITL and a default effect. Rule semantics come
from `internal/policy.Engine.Decide`. `policy.ValidatePredicate` runs before
construction so invalid fixture rows are errors rather than silently skipped.

There is no existing engine snapshot/export interface. `Engine.Revision`
fingerprints its configured authority, while `Exporter` is an asynchronous
side-effect interface for live decision events. The fixture command does not
attach an exporter or add a snapshot subsystem. Operators supply the effective
static rows they intend to compare. Duplicate replacing scopes are rejected.

`cmd/policy_test_cli.go` constructs the engine with `ForceEnable: true` and
explicit static fields only. It never calls the live config loader, `initPolicy`,
server construction, store initialization, connector registry, secret resolver
or tracing initialization. Webhook, audit, exporter and cache remain unset.
Decisions use a fresh background context with no recording span. The command
reads only its selected input and writes only its report/diagnostic streams.
Ambient backend URLs, auth keys, Vault and OTLP environment settings have no role.

All cases are validated before any decision is evaluated. Unknown/case-variant
schema fields and duplicate JSON keys fail closed. Nesting is checked before
the recursive unique-key decoder. `internal/jsonvalue` preserves exact numbers
and business-key spelling. Raw argument objects and predicate values are kept
separate from schema keys and never printed. Invalid-input diagnostics do not
echo argument contents. IDs and rule metadata are operator-declared/reportable,
not secret containers.

Scope validation is separate from reportable metadata validation. Declared
tenant/principal/agent/connector/tool values and tool lists retain their original
bytes, including outer whitespace, within the existing UTF-8, nonblank, control
and length bounds. Case/rule IDs, reason codes, effects and predicate syntax keep
the clean-string requirement. The fixture can therefore test exact padded
positive identities and the evaluator's unchanged conservative restrictions.
Exact grant duplicates remain errors. Mandatory-HITL duplicates are checked by
its legacy trimmed effective scope, so newly accepted padding cannot silently
replace an earlier restrictive declaration. No fixture mode or normalization
layer is added.

The report omits wall-clock latency, run IDs, random IDs, timestamps and free-form
reasons. It contains only version/revision, ordered case metadata, compared
decisions and counts. Process status distinguishes matching cases (0), mismatches
(1), invalid schema/usage (2), and input/output/internal failures (3). A failed
writer can leave a partial stdout stream; callers must check process status.

## Intentional limits

Fixture tenant/principal/agent values are declarations. No identity, run,
assignment, target revision or approval is authenticated or created. Static grant
matching is tenant/agent/connector based; declaring a principal does not add RBAC.
`pending` is the exact existing logical effect and does not persist HITL state.
`run.create` normally allows because this engine stage skips connector grants;
actual admission authority, kills and budgets live outside the fixture boundary.

The command does not read deployment configuration, secret stores, live overlays
or remote policies. It does not validate connector endpoints, execution sandbox
policy, runner ownership, current authority or business effects. Logical tests
and runtime qualification remain separate evidence.

The input is limited to one 1-MiB document, 32 container levels, 256 cases,
256 rows in each rule list, 64-KiB argument/value payloads, 256-byte bounded
identity/metadata strings and 128 distinct entries per tool list. The fixture
format is an explicit subset, not a lossless import/export of `langgraph.json`.
See the [user reference](../policy-fixtures.md).

## Verification

Tests exercise the real existing engine through all three effects, deny versus
pending precedence, tool deny versus mandatory review, tenant scope, session
and run-create stages, exact large numbers and string/number distinctions.
Malformed/unknown/duplicate/case-variant fields, invalid predicates and bounds
are refused before output. Mismatches keep later cases visible. Real-main
subprocess cases exercise a padded tenant/agent/connector grant, exact padded
tool allows, canonical neighbors that remain denied, and preserved padded
predicate, mandatory-review and tool-filter restrictions under default allow.
The parser's scope bytes are checked directly; padded diagnostic metadata,
control/blank/oversized scopes and replacing duplicate scopes remain invalid.

A network tripwire, hostile ambient URLs/keys, literal environment/Vault markers
and a file-content/directory comparison verify that the command neither calls
external services, resolves secret strings nor writes state during those cases.
Subprocess tests invoke the real `main` command dispatch and verify exit statuses
0 through 3. The example produces an identical report across repeated evaluations.
These checks demonstrate the declared offline boundary; they are not an OS
sandbox for arbitrary extensions added to this command in the future.
