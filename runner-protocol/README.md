# Runner Protocol

**Version:** 0.1.0 (draft) · **Spec:** [`PROTOCOL.md`](PROTOCOL.md)

How a Runkite control plane dispatches runs to workers and how workers stream results back. Transport-agnostic, language-agnostic, framework-agnostic. The control plane never imports an agent framework; a runner never serves Agent Protocol HTTP/SSE.

In-repo only for v0.3 — no separate protocol repo until an external implementer shows up.

## Contents

| Path | What |
|------|------|
| [`PROTOCOL.md`](PROTOCOL.md) | Normative contract (assignments, events, checkpoints, store, auth, …) |
| [`schemas/`](schemas/) | JSON Schema for `RunAssignment` / `RunEvent` |
| [`examples/`](examples/) | Golden lifecycle fixtures (HITL, cancel, tools, …) |
| [`tests/`](tests/) | Schema + lifecycle gate (`go test ./runner-protocol/tests/...`) |

## Conformance (one paragraph)

A runner is conformant when it can consume a `RunAssignment`, emit the documented `RunEvent` stream (including HITL interrupt/resume and cancel), and — if it claims LangGraph proxy checkpoints — honor §6 of `PROTOCOL.md` (run-bound `/internal/checkpoints/*`, ETag CAS, 16 MiB cap, soft-no-op on `403 run_not_inflight`). Fixture shapes are gated by `go test ./runner-protocol/tests/...`; live matrix proof is `make test-matrix` (Python + TypeScript LangGraph against SQLite/MySQL/Mongo/Postgres). Opaque checkpoint **envelopes** are shared across languages; serde payloads are not — do not cross-resume Python↔TypeScript threads (§6.3–6.4).

## Checkpoint extension (opaque proxy)

See [`PROTOCOL.md` §6](PROTOCOL.md#6-checkpoint-api) — dual mode (direct Postgres vs HTTP proxy), `/latest?ns=`, and [§6.4 threat model](PROTOCOL.md#64-threat-model-proxy-checkpoints) (run binding, assignment tenant, size limits, corrupt-blob behavior).
