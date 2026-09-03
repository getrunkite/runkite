# Kubernetes kind proof (K0–K3)

**Status:** kind-proven for packaging / ops shapes. **Not** Kubernetes Supported. **Not** an EKS (or other cloud) soak.

Compose multi soak (`make soak-multi`) remains the Supported multi-CP HA correctness proof. Cluster installs stay **Compatible** for production cloud claims until a named, budget-capped cloud soak (K4) is written up.

## Suite

| Make target | Shape | What it proves |
|-------------|--------|----------------|
| `kind-helm-smoke` | K0 | Empty kind → Postgres/Redis → `values-supported` Helm install → `/readyz` + one agent run |
| `kind-helm-rotate` | K1 | Runner-token dual allowlist + rolling CP/runner restart; runs succeed across the window |
| `kind-helm-reclaim` | K2 | Force-delete CP + runners mid-run → stale reclaim + same `run_id` reaches success |
| `kind-helm-net` | K3 | Ingress path succeeds; runner NetworkPolicy deny-inbound holds (kind ≥ 0.24 / kindnet) |

Run all four (sequential, tears down between scripts unless `KEEP_CLUSTER=1`):

```bash
make kind-helm-all
```

Prereqs: `kind`, `kubectl`, `helm`, `docker`, `curl`, `python3`, `openssl`. Not on PR CI (heavy).

## Latest local run

| Target | Result | When |
|--------|--------|------|
| `kind-helm-smoke` | PASS | 2026-09-03 |
| `kind-helm-rotate` | PASS | 2026-09-03 |
| `kind-helm-reclaim` | PASS | 2026-09-03 |
| `kind-helm-net` | PASS | 2026-09-03 |

## Non-claims

- Not Kubernetes **Supported** / not a published EKS/GKE/AKS soak (K4 — `$50–100` cap, tear down, written results — still open)
- Not a multi-hour kind soak
- Not cert-manager / public TLS / IRSA / ExternalSecrets productization
- Not MCP sticky Ingress beyond what `kind-helm-net` exercises
- Schema migrate Job is unnecessary for boot: `serve` applies numbered migrations on `Init` (same as binary)

## K4 (still open)

One real-cloud soak, **$50–100 budget**, tear down, runbook + results attached. Until that lands, announce and docs must keep cluster installs as **Compatible**.
