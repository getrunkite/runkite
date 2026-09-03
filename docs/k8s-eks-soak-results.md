# EKS K4 soak results

| Field | Value |
|-------|--------|
| Account | `797781631961` |
| Region | `eu-north-1` |
| Cluster | `runkite-k4` |
| Node type | `m7i-flex.large` × 2 (Free Tier–eligible; `t3.medium` was blocked) |
| Images | ECR `linux/amd64` (Apple Silicon default arm64 caused `exec format error`) |
| Smoke (UTC) | 2026-09-03T05:44Z |
| Torn down | 2026-09-03 ~05:50–06:00Z UTC (`make eks-down`; no clusters left; ECR repos deleted) |
| Approx spend | _(Billing next day; target ≪ $50)_ |
| `/readyz` + `echo_agent` | **PASS** (`run_id=0e914741-44bf-4f8b-826a-715e3c8d529c`) |
| CP pod kill + re-run | **PASS** (`run_id=7dcaa768-52ba-4e33-b4b2-396055e6dab1`) |
| Optional soak loop | skipped (finish-and-tear-down) |
| Known issues | Free Tier blocks non-eligible instance types; must build `--platform linux/amd64` from Apple Silicon |

**Claim:** kind K0–K3 + named EKS install/smoke/reclaim proven. Compose multi soak remains Supported multi-CP HA. Not claimed: IRSA, public TLS, multi-AZ HA soak, 24h soak.
