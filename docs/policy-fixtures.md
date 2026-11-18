# Offline policy fixtures

The `runkite policy test` command is available in v0.10.0 binaries. The example
fixture files remain source-checkout assets.

An administrator can check a proposed static connector policy against declared
expectations without creating runs or executing tools:

```sh
runkite policy test -f examples/policy_fixtures/refunds.json
```

The command writes a JSON report to stdout. Exit status is `0` when all cases
match, `1` when any expectation differs, `2` for invalid fixtures or command
usage, and `3` for input/output or internal failures. Errors go to stderr. Use
`-f -` to read JSON from stdin. No URL, API key, output-file or live-config flags
exist. The command does not load `langgraph.json`, read backend configuration,
resolve secrets, contact models/Vault/control planes, or write files or records.

This is separate from [`runkite sim`](sim.md). Live simulation creates real
managed runs and allowed cases can call real connectors. Offline policy tests
exercise logical decisions from a declared snapshot only.

## Fixture shape

A fixture is one strict UTF-8 JSON document:

```json
{
  "schema_version": 1,
  "policy": {
    "default_effect": "deny",
    "grants": [
      {
        "id": "ticket-read",
        "tenant_id": "acme",
        "agent_id": "ticket-agent",
        "connector": "tickets",
        "tools": {"allow": ["read"], "deny": ["delete"]}
      }
    ]
  },
  "cases": [
    {
      "id": "allowed-ticket-read",
      "input": {
        "stage": "tool.call",
        "tenant_id": "acme",
        "principal": "declared-alice",
        "agent_id": "ticket-agent",
        "connector": "tickets",
        "tool": "read",
        "args": {"ticket_id": "fixture-123"}
      },
      "expect": {"effect": "allow", "rule_id": "ticket-read"}
    }
  ]
}
```

`policy` must be an explicit object. An empty object means an enabled policy
with the engine's default deny; it does not model a deployment with policy
absent. `default_effect` is `allow` or `deny`, defaulting to `deny`.

The supported snapshot fields are:

- `grants`: existing `id`, `tenant_id`, `agent_id`, `connector` and optional
  `tools.allow`/`tools.deny`. Tenant, agent and connector are required. Tool deny
  wins over allow. As in the live engine, an absent filter or empty allow list
  does not mean deny-all. Remove a grant or specify denied tools as appropriate.
- `predicates`: existing argument checks with `id`, `tenant_id`, `connector`,
  optional `agent_id`/`tool`, `when: {path, op, value}`, `effect` and optional
  `reason_code`. Effects are `deny` or `pending`. Operators are `eq`, `neq`,
  `gt`, `gte`, `lt`, `lte`, `in`, `contains` and `exists`. `value` is required
  except for `exists`. Arguments and values retain exact JSON numbers and types;
  strings are not coerced to numbers. Missing paths follow the existing engine.
- `mandatory_hitl`: existing `id`, `tenant_id`, optional `agent_id`, `connector`
  and optional `tools`. Matching allows become `pending`; denials stay denied.

The fixture represents a controlled static snapshot. It does not query Admin
or merge database overlays. Duplicate exact grant scopes and duplicate effective
mandatory-HITL scopes are rejected instead of silently replacing earlier
declarations. Mandatory-HITL uses the engine's legacy trimmed scope for this
check; positive grants use exact scope bytes. Predicate IDs
and case IDs must be unique. Invalid predicates are errors, even though the
live engine's general constructor can skip invalid predicate rows.

Unknown fields, case variants of schema keys, duplicate JSON keys, additional
JSON documents and invalid types are rejected before evaluating any case.
Schema fields cannot be `null`; omit optional fields instead. Business values
inside `args` or `when.value` can still contain JSON null.
There are no includes or executable references. `${ENV}`, `env:`, `file:` and
`vault:` strings in business arguments are literal data.

## Cases and expectations

Every input requires a declared `tenant_id`, `principal` and `agent_id`:

- `tool.call` requires `connector`, `tool` and object `args`, including `{}`.
- `connector.session` requires `connector` and excludes `tool` and `args`.
- `run.create` excludes connector/tool/args. The static engine skips connector
  grants for this stage and normally allows it. This does not test actual run
  admission, authentication, agent existence, kill switches, limits or budgets.

Declared tenant, principal, agent, connector and tool values retain surrounding
whitespace. Tool lists retain it too. Required scope values must contain a
non-whitespace character, but the parser never rewrites them. This lets a fixture
distinguish a grant for `" echo "` from the neighboring stored ID `"echo"`.
For example, with only an unrestricted grant for tenant `"acme"`, agent
`" echo "` and connector `"tools"`, a connector-session case for `" echo "`
expects `allow`; the same case for `"echo"` expects `deny` with
`policy_no_grant`.

The current evaluator's restrictive compatibility behavior remains visible:
padded predicate and mandatory-HITL scopes can still deny or require review for
a canonical neighboring spelling. Legacy per-grant tool-filter denials can also
apply across trim-equivalent scopes, including under default allow. An ambiguous
padded configuration may therefore deny more. Fixtures use the real evaluator;
accepting a padded declaration does not invent an exact-only restriction mode or
prove that a live API accepts that identity. See
[policy identity upgrade guidance](ops-runbook.md#unreleased-policy-identity-upgrade).

The principal is fixture data, not a verified identity. Static grants do not
supply principal-specific RBAC. Declaring `principal: "admin"` does not simulate
an authenticated Admin role or prove that the caller could invoke an API.

`expect.effect` is required and must be the exact engine value `allow`, `deny`
or `pending`. Optional `reason_code` and `rule_id` are compared exactly when
present. An explicit empty string checks that the value is empty. Omission
skips that comparison. Every valid case is evaluated, including after a mismatch.
A `pending` result creates no approval or capability and performs no polling.

The report contains the engine's policy revision, ordered case IDs, expected and
actual effect/reason-code/rule-ID fields, and pass/fail counts. It omits input,
arguments, principal, free-form reasons and timing. Keep secrets out of fixture
metadata such as IDs and reason codes because those fields are reported. The
same fixture bytes produce the same report with the same source implementation.
A policy revision fingerprints this static configuration, not an upstream
service implementation or deployed policy state.

## Bounds and limits

The document limit is 1 MiB, nesting is at most 32 JSON containers, and there
are at most 256 cases and 256 entries in each rule list. Each argument object
and predicate value is at most 64 KiB. Identity/scope/metadata strings are at
most 256 UTF-8 bytes without control characters. Declared scope strings preserve
outer whitespace; case/rule IDs, reason codes, effects and predicate syntax
remain clean without outer whitespace. Tool lists contain at most 128 distinct
exact names each. Empty optional scopes retain their existing wildcard meaning;
blank nonempty scope values are rejected.

Unsupported fields include webhooks, SIEM/audit configuration, approval quorum,
cache settings, connectors, credential references as configuration, live
overlays and target/schema revisions. Test those integration boundaries through
existing dedicated tests and qualification flows. This command does not prove
connector transport enforcement, current workload authority, execution
containment, secret handling or downstream business effects.

See the [complete example](../examples/policy_fixtures/refunds.json) for
allow/deny/pending, mandatory review, numeric types, tenant separation and
`run.create` boundaries. The [engineering note](internal/offline-policy-fixtures.md)
records the isolation and implementation choices.
