# Fixture replay (`runkite sim`)

Replay YAML cases against a **live** control plane. This is CI for connector policy, not a simulation lab: the runner, model, and MCP servers are the real ones. The plane only marks the run so FinOps and daily admission skip; everything else still happens.

```bash
runkite sim -f fixtures.yaml \
  --url http://127.0.0.1:2026 \
  --api-key "$RUNKITE_API_KEY" \
  --timeout 2m
```

`--url` falls back to `RUNKITE_URL` (default `http://127.0.0.1:2026`). `--api-key` to `RUNKITE_API_KEY`. `--tenant` to `RUNKITE_TENANT` or a file-level `tenant`.

Stdout is the operator surface (no JSON this release):

```
PASS  small-transfer   run_id=...  1.2s
FAIL  large-transfer   run_id=...  expected pending on bank/transfer, got allow
2 passed, 1 failed
```

Exit `0` only when every case passes. CI can use the exit code or grep `PASS` / `FAIL`.

## Header and who may set it

Each case:

1. `POST /threads` with `{"if_exists": "do_nothing"}` (fresh thread; HITL/state cannot leak across cases).
2. `POST /threads/{id}/runs` with the case `input`, header `X-Runkite-Simulation: true`.
3. Poll `GET /runs/{id}` every 1s until a terminal status or `--timeout`.
4. If `expect.policy_effects` is set: `GET /admin-api/audit-events?run_id={id}&limit=200` and match.

The header is **not** a `langgraph.json` key. Body `metadata` cannot turn a run into a simulation. A write-only key gets `403` (`reason_code: simulation_requires_admin`). Auth off (local `dev`) can set the flag.

**CI blast radius:** this release uses an **admin** key for `sim`. That credential can also cancel runs, edit grants, hit kill/break-glass, and read spend. Scope the secret like any other Admin credential, not like a run-only token.

## What skips vs what stays

| Skips on `metadata.simulation=true` | Still enforced |
|-------------------------------------|----------------|
| FinOps reservation hold on create | Concurrent admission (`max_concurrent_runs`) |
| Terminal usage ingest | Rate limits |
| `CountRunsSince` / UTC-day `max_runs_per_day` | Kill switches, connector policy, HITL pending |
| LLM response cache on create | Cancel / reclaim |

A2A children inherit the flag from the parent. Cron and runner-token paths never set the header themselves.

Admin → Runs / Run detail show a `sim` badge when the flag is set. No extra filter this release.

## Fixture YAML

See [`examples/sim/predicates.yaml`](../examples/sim/predicates.yaml).

- Top-level `agent` is required unless every case sets `agent`.
- `input` is the Agent Protocol run input object, passed through as JSON.
- Unknown keys at the file, case, expect, or policy-effect level **fail parse** (a typo must not silently pass).
- `expect.status` is optional (`success` / `error` / `timeout` / `interrupted`). Default: any terminal.
- `expect.policy_effects` is optional. When present (including `[]`), compared as **set equality** on `(connector, tool, effect)` against audit rows whose `decision` is `deny` or `pending` and `action` is `tool.call`. `reason_code` is compared only when the fixture named one. Extra deny/pending fails the case. Allows are ignored.
- **Pending does not auto-approve.** A case that expects `pending` passes when audit shows pending, even if the run later errors because the tool was refused. Approve stays in Admin.
- `expect.policy_effects` needs a SQL state backend. Mongo: that case fails with `audit search needs SQL`.

## Honesty

- Not a sandbox. In-graph tools that never go through connector MCP stay ungoverned.
- Allow cases hit the **real** MCP server / SaaS.
- Downstream is not stubbed. If the model never calls `bank/transfer`, a pending expect fails (`got allow`).
- This is not Collinear-style world simulation.
