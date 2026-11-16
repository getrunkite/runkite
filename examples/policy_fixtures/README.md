# Static policy expectations

Unreleased, source checkout only. Published v0.9.0 does not include this command.

From a source checkout:

```sh
go build -o /tmp/runkite-source ./cmd
/tmp/runkite-source policy test -f examples/policy_fixtures/refunds.json
```

The nine cases use synthetic tenant/agent/tool data. They exercise ordinary
allow, predicate review, deny precedence, tool deny, mandatory review, tenant
separation, session grants, numeric types and the policy-only run-create stage.
No tool or model executes. No approval record is created.

Edit a grant or expectation and rerun. Exit `1` means at least one mismatch;
`2` means invalid input/usage and `3` means an input/output/internal error.
The complete [CLI reference](../../docs/policy-fixtures.md) defines the strict
fixture subset, output and limits. Live [`runkite sim`](../../docs/sim.md) is a
separate integration flow that can execute real allowed connectors.
