# Runner Protocol

**Version:** 0.1.0 (draft) · **Spec:** [`PROTOCOL.md`](PROTOCOL.md)

How a Runkite control plane dispatches runs to workers and how workers stream
results back. Transport-agnostic, language-agnostic, framework-agnostic. The
control plane never imports an agent framework; a runner never serves Agent
Protocol HTTP/SSE.

**Published:** [github.com/getrunkite/runner-protocol](https://github.com/getrunkite/runner-protocol)
(mirror). **Canonical edits** land in the
[Runkite](https://github.com/getrunkite/runkite) monorepo under `runner-protocol/`
and are mirrored here on `main`. Spec issues/PRs are welcome in this repo;
Runkite *implementation* issues stay on `runkite`.

## Contents

| Path | What |
|------|------|
| [`PROTOCOL.md`](PROTOCOL.md) | Normative contract (assignments, events, checkpoints, store, auth, …) |
| [`schemas/`](schemas/) | JSON Schema for `RunAssignment` / `RunEvent` |
| [`examples/`](examples/) | Golden lifecycle fixtures (HITL, cancel, tools, …) |
| [`tests/`](tests/) | Schema + lifecycle gate (`go test ./...` from this repo root) |

## Conformance (one paragraph)

A runner is conformant when it can consume a `RunAssignment`, emit the
documented `RunEvent` stream (including HITL interrupt/resume and cancel), and
— if it claims LangGraph proxy checkpoints — honor §6 of `PROTOCOL.md`
(run-bound `/internal/checkpoints/*`, ETag CAS, 16 MiB cap, soft-no-op on
`403 run_not_inflight`). Fixture shapes are gated by `go test ./...` in this
repo; live matrix proof lives in Runkite (`make test-matrix` — Python +
TypeScript LangGraph against SQLite/MySQL/Mongo/Postgres). Opaque checkpoint
**envelopes** are shared across languages; serde payloads are not — do not
cross-resume Python↔TypeScript threads (§6.3–6.4).

## Extensions

| Extension | Spec | Notes |
|-----------|------|--------|
| **Opaque checkpoint proxy** | [`PROTOCOL.md` §6](PROTOCOL.md#6-checkpoint-api) | Dual mode (direct Postgres vs HTTP proxy); [`§6.4` threat model](PROTOCOL.md#64-threat-model-proxy-checkpoints) (run binding, assignment tenant, size limits, corrupt-blob behavior) |
| **Store API** | [`PROTOCOL.md` §7](PROTOCOL.md#7-store-api) | Run-bound key/value for frameworks that need durable store access via the plane |

Live end-to-end proof of these extensions against Runkite’s control plane is
`make test-matrix` in the monorepo — not duplicated in this repo.

## Develop / republish

```bash
# From this clone (or from runkite/runner-protocol after a mirror):
go test ./...

# Manual republish from the Runkite monorepo (CI also does this on main):
git subtree split --prefix=runner-protocol -b protocol-publish
git push git@github.com:getrunkite/runner-protocol.git protocol-publish:main
```
