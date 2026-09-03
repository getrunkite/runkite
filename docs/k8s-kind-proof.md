# Kubernetes kind proof (K0–K3)

**Status:** kind-proven for packaging / ops shapes. **Not** Kubernetes Supported. **Not** an EKS (or other cloud) soak.

Compose multi soak (`make soak-multi`) remains the Supported multi-CP HA correctness proof. Named EKS smoke/reclaim is recorded in [k8s-eks-soak-results.md](k8s-eks-soak-results.md); treat full cloud HA claims cautiously until a longer soak exists.

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

- Not a multi-hour / multi-AZ EKS HA soak (K4 smoke only — see [k8s-eks-soak-results.md](k8s-eks-soak-results.md))
- Not a multi-hour kind soak
- Not cert-manager / public TLS / IRSA / ExternalSecrets productization
- Not MCP sticky Ingress beyond what `kind-helm-net` exercises
- Schema migrate Job is unnecessary for boot: `serve` applies numbered migrations on `Init` (same as binary)

## K4 (EKS smoke — done 2026-09-03)

Named EKS install + `/readyz`/`echo_agent` + CP pod kill reclaim on `eu-north-1`, then tear-down: [k8s-eks-soak-results.md](k8s-eks-soak-results.md). Runbook: [k8s-eks-soak.md](k8s-eks-soak.md). Not a multi-hour cloud HA soak.

