# Kubernetes EKS soak (K4)

**Status:** runbook ready — execute only with AWS credentials + budget alarm.  
**Cap:** keep the live cluster under **~$50–100** of the free credit (you have ~$200). Tear down the same day.  
**Claim after green:** cluster installs move from Compatible packaging toward a **named EKS soak proof** (still not “every cloud forever”).

Kind K0–K3: [k8s-kind-proof.md](k8s-kind-proof.md). Compose multi soak remains Supported multi-CP HA.

## Do not

- Click **Create cluster** in the EKS console wizard (harder to tear down cleanly; easy to leave NAT/ALB running).
- Wait for Hyderabad (`ap-south-2`) — use **`eu-north-1` (Stockholm)** where the console already is.
- Leave the cluster overnight “to see.” EKS control plane alone is ~$0.10/hr; nodes + accidental NAT/ALB add up.
- Use RDS + ElastiCache for this soak — in-cluster Postgres/Redis (same as kind) is enough and cheaper.

## What we prove on EKS

| Step | Proof |
|------|--------|
| Install | Helm `values-supported` on real EKS + ECR images → `/readyz` + `echo_agent` success |
| Reclaim | Force-delete a CP pod mid-run → reclaim + run success (K2 shape on cloud) |
| Optional short soak | Loop create/run for ~30–60 min; no Sev-1; note known issues |

Not claimed: IRSA, cert-manager public TLS, multi-AZ HA, ExternalSecrets, 24h soak.

## One-time laptop setup

```bash
# Already installed via brew if you followed the agent setup:
aws --version
eksctl version
kubectl version --client
helm version

# Console user → access keys or SSO. Pick ONE:
aws configure          # Access key ID + secret; default region eu-north-1
# or: aws configure sso

aws sts get-caller-identity   # must print your account id (console shows it under the username)
```

**Billing guard (do this before create):** AWS Console → Billing → Budgets → create a **$50** actual-cost alarm email to yourself.

## Day-of commands (order)

```bash
# 0) Confirm CLI auth (do not Create cluster in the console).
aws sts get-caller-identity

# 1) Create cluster (~15–20 min). Spend clock starts here.
make eks-up

# 2) Build/push images to ECR, install Helm, smoke + CP pod kill.
#    KEEP_NS=1 leaves the namespace for the optional soak loop.
KEEP_NS=1 make eks-smoke

# 3) OPTIONAL: longer loop (default 30 min). Ctrl-C is fine; still tear down.
SOAK_MINUTES=30 make eks-soak-loop

# 4) ALWAYS tear down when done (or if anything looks wrong).
make eks-down
```

Expected wall clock: **~1–2 hours** create → prove → delete. Expected spend if you stay disciplined: **well under $20**; the $50–100 cap is headroom for mistakes (forgotten NAT, leftover LB).

## Tear-down checklist

After `make eks-down`:

```bash
eksctl get cluster --region eu-north-1
# should not list runkite-k4

aws elbv2 describe-load-balancers --region eu-north-1 --query 'LoadBalancers[].LoadBalancerName'
aws ec2 describe-nat-gateways --region eu-north-1 --filter Name=state,Values=available
aws ecr describe-repositories --region eu-north-1 --query 'repositories[?starts_with(repositoryName, `runkite`)].repositoryName'
```

Delete leftover ALBs/NAT/ECR repos if any. Confirm Billing → Costs by service next day is flat.

## Results

Paste PASS/FAIL + timestamps into [k8s-eks-soak-results.md](k8s-eks-soak-results.md) (created by the smoke script on success, or write by hand). Then update `docs/limitations.md` / announce gate only after results exist and the cluster is gone.
