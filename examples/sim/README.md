# Fixture replay examples

`runkite sim` drives a live control plane. It is not a lab and does not stub MCP.

```bash
runkite sim -f examples/sim/predicates.yaml \
  --url http://127.0.0.1:2026 \
  --api-key "$RUNKITE_API_KEY" \
  --timeout 2m
```

Use an **admin** key when client auth is on (`X-Runkite-Simulation` is admin-gated). That key can also hit Admin routes (kill, grants, spend), so treat CI secrets accordingly.

`predicates.yaml` matches the payroll / bank / transfer shape in the Grants docs. Change `agent` (and the prompts) to a graph you actually run. Allow cases call the real connector.

A case that expects `pending` passes when Admin audit has that deny/pending row. It does not approve the HITL row for you.

Policy asserts need a SQL state backend. Mongo returns a clear CLI failure for `expect.policy_effects`.
