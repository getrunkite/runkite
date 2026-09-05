# Pre-v0.3 manual QA playbook

Use this doc before tagging `v0.3.0`. It covers everything you can reasonably exercise by hand on a laptop. Automated CI (`make test`, `make test-matrix`, `make smoke-governance`, EKS smoke) covers scale, backend goldens, and paid infra — not duplicated here except as a sign-off list.

**Interactive checklist:** after `./start.sh`, open **http://127.0.0.1:3100/** — the QA hub loads the same steps as checkboxes (persisted in your browser). Machine-readable matrix: `qa-matrix.json`.

---

## 0 · Before you start

### Prerequisites

| Item | Check |
|------|--------|
| Repo built | `./runkite` exists (`go build -o ./runkite ./cmd` if needed) |
| Gemini key | Copy `.env.llm.example` → `.env.llm`, set `GOOGLE_API_KEY` |
| Python venv | `python/.venv` with LLM deps (`start.sh` installs if missing) |
| Optional adapters | CrewAI / LlamaIndex / AutoGen venvs under `python/adapters/*` |
| Optional TS | `typescript/runkite-runner/node_modules` + `examples/gemini/langgraphjs_agent/node_modules` |
| Port 2026 free | Stop any other local `runkite serve` or docker stack using `:2026` |

### How to record results

Keep a simple log (Notes app is fine):

```
[date] [profile] [feature id] PASS/FAIL — one-line note
```

When something fails, capture: Admin run id, browser console, and `examples/gemini/dogfood/logs/*.log`.

---

## 1 · Default stack — Gemini dogfood (SQLite · dev)

This is your **primary** profile: real Gemini traffic, all framework UIs, FinOps, HITL checkpoint, Admin UI on SQLite.

```bash
./examples/gemini/dogfood/start.sh
```

| URL | Purpose |
|-----|---------|
| http://127.0.0.1:3100/ | **QA hub** — checklist + backend matrix |
| http://127.0.0.1:3100/index.html | Hub chat (pick any agent) |
| http://127.0.0.1:3101/ … :3107/ | Per-framework locked chats |
| http://127.0.0.1:2026/admin/ | Admin UI |
| http://127.0.0.1:2026/admin/spend | FinOps |

Stop: `./examples/gemini/dogfood/stop.sh`

Clean restart (wipes local dogfood DB):

```bash
./examples/gemini/dogfood/stop.sh
rm -rf examples/gemini/dogfood/.run examples/gemini/dogfood/logs
./examples/gemini/dogfood/start.sh
```

### 1.1 Framework matrix (one prompt each)

On each port, send: *"Reply in one sentence: what framework are you?"*

| Port | Agent | Pass |
|------|-------|------|
| 3101 | gemini_langgraph | AI reply + usage JSON in side panel |
| 3102 | gemini_langchain | Same |
| 3103 | gemini_crewai | Same (or skip if venv missing) |
| 3104 | gemini_llamaindex | Same |
| 3106 | gemini_autogen | Same |
| 3107 | gemini_langgraphjs | Same |

Then:

```bash
./examples/gemini/dogfood/smoke_usage.sh
```

**Pass:** script exits 0; every agent reports `Output.usage` with tokens + model id.

### 1.2 Thread history recall (your Gemini audit test)

Use **http://127.0.0.1:3101/** (LangGraph). Send these **on the same thread** (do not refresh):

1. `What is the capital of France?`
2. `What language do they speak there?`
3. `Name one famous landmark in that city.`
4. Click **History test** (or send manually):  
   `List every question I asked you in this conversation, in order, quoting my exact wording.`

**Pass:**

- Reply lists all three earlier questions in order.
- **Question log** panel in the UI matches (use **Copy**).
- Optional external check: paste the copied log into a separate Gemini chat and ask *"Did I miss any questions?"*

**Admin check:** Threads → open your thread → runs list shows four runs on one thread.

### 1.3 HITL approve / deny (checkpoint, not email)

**http://127.0.0.1:3105/** — `approval_agent` simulates *send email to alice@example.com*. No real SMTP is configured; this tests **interrupt → resume**, not delivery.

**Approve path**

1. Send any message (e.g. *"Send the weekly report"*).
2. Wait for yellow HITL bar → click **Approve**.
3. **Pass:** final message *"Email sent to alice@example.com successfully!"*

**Deny path**

1. Refresh page (new thread) or switch agent and back.
2. Same prompt → click **Deny**.
3. **Pass:** *"Action was not approved. Cancelled."*

**Admin:** Runs → interrupted run visible; resumed run completes with success.

> **Real email on approve/deny** is not part of dogfood. That requires connector + webhook/SMTP integration in your own `langgraph.json`. The shipped demo proves checkpoint HITL only. Connector HITL with Admin **Pending** queue is covered in §5.2 (Postgres profile).

---

## 2 · Admin UI — every page

Login is skipped in dev (no auth configured). Walk every nav item.

| Page | What to verify |
|------|----------------|
| **Overview** | Counts > 0 after dogfood runs |
| **Agents** | `gemini_*`, `approval_agent` listed; tenant `default` |
| **Agent detail** | Schema loads; version history tab |
| **Try agent** | Pick `gemini_langgraph`, prompt, live SSE + usage in raw panel |
| **Registry** | Page loads (may be empty) |
| **Threads** | Dogfood threads listed |
| **Thread detail** | Runs on thread match your chat |
| **Runs** | Success + interrupted statuses |
| **Run detail** | SSE replay; **Run manifest** card (agent, tenant, policy labels) |
| **Connectors** | Page loads |
| **Cron** | Schedules if configured |
| **Webhooks** | Dead-letter list loads |
| **Grants** | List + create form (SQL backend) |
| **Mandatory HITL** | List + create form |
| **Pending** | Queue UI (empty unless connector HITL fired) |
| **Kill** | Create pause/kill; clear afterward |
| **Break-glass** | Mint window (reason + expiry ≤24h); revoke |
| **Audit** | Search returns policy decisions after governance tests |
| **Spend** | See §3 |

**Try agent vs dogfood UIs:** both hit the same control plane. Try agent is the Admin-native client; dogfood ports are framework-specific playgrounds.

---

## 3 · FinOps — spend, alerts, budgets

Open **http://127.0.0.1:2026/admin/spend**

### 3.1 Basic metering

After several Gemini prompts:

- **Est. USD**, tokens in/out, run count > 0
- Filter tenant `default`, agent `gemini_langgraph` — totals narrow
- **Export CSV** and **Export JSON** — files download and contain your runs

### 3.2 Soft budget alert

Dogfood config sets `soft_pct: 50` and `max_usd_per_day: 5` in `00_controlplane.json`. To trigger faster:

1. Spend → **Live FinOps config** → tenant `default` → set `max_usd_per_day` to **`0.0001`**, keep **soft** enabled → Save.
2. Send ~5–10 Gemini prompts from dogfood.
3. Refresh Spend → **Alerts** table.

**Pass:** row with reason `budget_soft` and/or `budget_alert`.

4. Restore budget (e.g. `5`) when done.

### 3.3 Unpriced / unmetered (optional)

- **Unpriced:** remove a model from pricebook overlay, run that model → `usage_unpriced` alert.
- **Unmetered:** run `unknown_provider_agent` via Try agent (registered in dogfood CP config) → `usage_unmetered` alert.

Docs: [FinOps guide](https://getrunkite.github.io/runkite/support/finops.html)

---

## 4 · Backend matrix (SQL · NoSQL · Redis · no Redis)

Stop dogfood before binding another process to `:2026`.

```bash
./examples/gemini/dogfood/stop.sh
```

Start test infra once:

```bash
make infra-up   # Postgres :5433, Redis :6380, Mongo :27018
```

| Profile | State | Transport | Start (from repo root) | Governance | FinOps |
|---------|-------|-----------|------------------------|------------|--------|
| **sqlite_dev** | SQLite | in-process | `./examples/gemini/dogfood/start.sh` | Full | Full |
| **postgres_redis** | Postgres | Redis | See below | Full | Full |
| **postgres_inprocess** | Postgres | in-process | `REDIS_URL=` + same DSN | Full | Full |
| **mongo_redis** | Mongo | Redis | See below | **501** on Admin gov | Partial |
| **docker_dev** | SQLite | in-process | `docker compose -f docker-compose.dev.yml up -d` | Limited | Limited |
| **multi_cp** | Postgres | Redis | `make smoke-multi` (needs `RUNNER_TOKEN`) | Full | Full |

**Postgres + Redis (prod-like single CP):**

```bash
POSTGRES_DSN='postgres://runkite:runkite@127.0.0.1:5433/runkite_test?sslmode=disable' \
REDIS_URL='redis://127.0.0.1:6380' \
./runkite serve --config examples/all_agents/langgraph.json --port 2026
```

Start a runner in another terminal:

```bash
python/.venv/bin/python -m runkite_runner \
  --config examples/all_agents/langgraph.json \
  --grpc-address 127.0.0.1:50051
```

**Pass:** `/readyz` → 200; Admin → Try → `echo_agent` → one prompt → success.

**Mongo + Redis:**

```bash
MONGO_URI='mongodb://127.0.0.1:27018/runkite_test?replicaSet=rs0&directConnection=true' \
REDIS_URL='redis://127.0.0.1:6380' \
./runkite serve --config examples/all_agents/langgraph.json --port 2026
```

**Pass:** Runs/threads work; Admin → Grants / Pending / Audit show **SQL required** empty state (501), not a crash.

Teardown: `make infra-down` (if you used `infra-up`).

**Automated full matrix:** `make test-matrix` — every framework × four backend cells; run before release if you have time (needs infra + venvs).

---

## 5 · Governance (SQL backends)

### 5.1 Automated smoke (required)

```bash
make smoke-governance
```

**Pass:** exit 0 — run binding, connector deny + audit, mandatory HITL one-shot, kill switch admission.

### 5.2 Connector HITL + Pending queue (manual, optional)

Requires Postgres profile + connectors configured in `langgraph.json` + mandatory HITL rule. Follow [Grants & HITL](https://getrunkite.github.io/runkite/support/grants.html) and [HITL ops](https://getrunkite.github.io/runkite/support/hitl-ops.html).

**Pass:**

1. Tool call triggers pending action.
2. Admin → **Pending** → **Approve** or **Deny**.
3. Admin → **Audit** shows decision.
4. Run completes or fails per policy.

> There is no built-in “email me when I approve” in the open-source stack. Wire a `webhooks` entry for `policy_decision` / `interrupt` to your own endpoint (Slack, email gateway) if you need notifications — see [Webhooks](https://getrunkite.github.io/runkite/support/webhooks.html).

### 5.3 Kill switch + break-glass (manual)

On SQL backend (dogfood SQLite is enough):

1. **Kill** → pause tenant `default` or agent `gemini_langgraph`.
2. Try agent or dogfood → run blocked or fails cleanly.
3. Delete kill switch → runs work again.
4. **Break-glass** → mint 1h window with reason → policy bypass for connector rules only (kill/auth still apply) → revoke.

---

## 6 · Platform extras

| Feature | How to spot-check |
|---------|-------------------|
| **Run manifest** | Run detail card after any post-v0.3 run |
| **Cache hit** | Enable `llm_cache` on an agent; identical input twice → second run `cache_hit` in manifest |
| **Cancel run** | Admin run detail → Cancel while slow agent running (use `slow_agent` on all_agents stack) |
| **Webhook dead-letter** | Point webhooks at `http://127.0.0.1:9` → failed delivery → Admin Webhooks → Redeliver (unsigned — known limitation) |
| **Registry CRUD** | Admin Registry → publish/edit/delete test entry |
| **Multi-CP** | `make smoke-multi` → Admin overview `cron_schedule_count: 1` → echo run success |

---

## 7 · What not to manual-test

Leave to CI / paid infra:

| Command | Covers |
|---------|--------|
| `make test` | Unit + protocol fixtures |
| `make smoke-governance` | Governance phases 0–3 |
| `make smoke-multi` | 3× CP + Redis queue |
| `make test-matrix` | Framework × backend goldens |
| `make test-llm-matrix` | Live Gemini N×N bench |
| EKS smoke | `docs/k8s-eks-soak.md` |

---

## 8 · Sign-off

When every checkbox in the QA hub is ticked (or every section above is PASS):

- [ ] Gemini dogfood: all frameworks + `smoke_usage.sh`
- [ ] History recall scenario PASS
- [ ] HITL approve + deny PASS
- [ ] Admin: every page loads; Try agent + run manifest verified
- [ ] FinOps: spend totals + at least one budget alert
- [ ] `make smoke-governance` PASS
- [ ] At least one non-SQLite profile spot-checked (Postgres+Redis recommended)
- [ ] Mongo profile: 501 empty states confirmed (optional but recommended)

Then proceed to **v0.3.0 tag + announce** (`docs/announce-v0.3-draft.md`).

---

## Quick reference — URLs after `./start.sh`

```
QA hub          http://127.0.0.1:3100/
Admin           http://127.0.0.1:2026/admin/
Spend           http://127.0.0.1:2026/admin/spend/
Try agent       http://127.0.0.1:2026/admin/try
LangGraph chat  http://127.0.0.1:3101/
HITL chat       http://127.0.0.1:3105/
```
