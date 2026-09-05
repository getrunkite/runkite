import { useCallback, useEffect, useMemo, useState } from "react";
import { api, ApiError } from "../api/client";
import type { AdminFinOpsView, FinOpsBudgetCap, FinOpsConfigPayload, FinOpsModelPrice } from "../api/types";
import { Button } from "../components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../components/ui/dialog";
import { Input } from "../components/ui/input";

type PriceRow = { model: string; input: string; output: string };
type BudgetRow = { key: string; usd: string; soft: boolean };

function pricebookToRows(pb: Record<string, FinOpsModelPrice> | undefined): PriceRow[] {
  const entries = Object.entries(pb ?? {});
  if (entries.length === 0) return [{ model: "", input: "", output: "" }];
  return entries.map(([model, p]) => ({
    model,
    input: String(p.input_per_1k ?? ""),
    output: String(p.output_per_1k ?? ""),
  }));
}

function budgetsToRows(caps: Record<string, FinOpsBudgetCap> | undefined, emptyKey: string): BudgetRow[] {
  const entries = Object.entries(caps ?? {});
  if (entries.length === 0) return [{ key: emptyKey, usd: "", soft: false }];
  return entries.map(([key, c]) => ({
    key,
    usd: c.max_usd_per_day != null ? String(c.max_usd_per_day) : "",
    soft: Boolean(c.soft),
  }));
}

function countModels(cfg: FinOpsConfigPayload | undefined): number {
  return Object.keys(cfg?.pricebook ?? {}).length;
}

function countCaps(caps: Record<string, FinOpsBudgetCap> | undefined): number {
  return Object.keys(caps ?? {}).length;
}

function summarizeSide(label: string, cfg: FinOpsConfigPayload | undefined): string {
  const models = countModels(cfg);
  const tenants = countCaps(cfg?.budgets?.tenants);
  const agents = countCaps(cfg?.budgets?.agents);
  return `${label}: ${models} model${models === 1 ? "" : "s"}, ${tenants} tenant cap${tenants === 1 ? "" : "s"}, ${agents} agent cap${agents === 1 ? "" : "s"}`;
}

function pickEditable(
  overlay: Record<string, FinOpsBudgetCap> | undefined,
  file: Record<string, FinOpsBudgetCap> | undefined,
): Record<string, FinOpsBudgetCap> {
  if (overlay && Object.keys(overlay).length > 0) return overlay;
  return { ...(file ?? {}), ...(overlay ?? {}) };
}

/** Guided live editor for FinOps overlays (pricebook + tenant/agent budgets). */
export function FinOpsConfigPanel({ seedModels = [] }: { seedModels?: string[] }) {
  const [view, setView] = useState<AdminFinOpsView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveOk, setSaveOk] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [prices, setPrices] = useState<PriceRow[]>([{ model: "", input: "", output: "" }]);
  const [tenantBudgets, setTenantBudgets] = useState<BudgetRow[]>([{ key: "default", usd: "", soft: false }]);
  const [agentBudgets, setAgentBudgets] = useState<BudgetRow[]>([{ key: "default/agent", usd: "", soft: false }]);
  const [freeConfirm, setFreeConfirm] = useState<string[] | null>(null);
  const [clearConfirm, setClearConfirm] = useState(false);

  const load = useCallback(async () => {
    setLoadError(null);
    try {
      const v = await api.get<AdminFinOpsView>("/finops");
      setView(v);
      const overlayPb = v.overlay?.pricebook ?? {};
      const effPb = { ...(v.file?.pricebook ?? {}), ...overlayPb };
      setPrices(pricebookToRows(Object.keys(overlayPb).length > 0 ? overlayPb : effPb));
      setTenantBudgets(
        budgetsToRows(pickEditable(v.overlay?.budgets?.tenants, v.file?.budgets?.tenants), "default"),
      );
      setAgentBudgets(
        budgetsToRows(pickEditable(v.overlay?.budgets?.agents, v.file?.budgets?.agents), "default/agent"),
      );
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : String(e);
      setLoadError(msg);
      setView(null);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (seedModels.length === 0) return;
    setPrices((rows) => {
      const have = new Set(rows.map((r) => r.model.trim()).filter(Boolean));
      const add = seedModels.filter((m) => m && !have.has(m));
      if (add.length === 0) return rows;
      return [
        ...rows.filter((r) => r.model.trim() || r.input || r.output),
        ...add.map((m) => ({ model: m, input: "", output: "" })),
      ];
    });
  }, [seedModels]);

  const diffNote = useMemo(() => {
    if (!view) return null;
    const fileModels = countModels(view.file);
    const effModels = countModels(view.effective);
    const fileTenants = countCaps(view.file?.budgets?.tenants);
    const effTenants = countCaps(view.effective?.budgets?.tenants);
    const fileAgents = countCaps(view.file?.budgets?.agents);
    const effAgents = countCaps(view.effective?.budgets?.agents);
    const same = fileModels === effModels && fileTenants === effTenants && fileAgents === effAgents && !view.has_overlay;
    if (same) return "Effective matches file baseline (no live overlay).";
    return "Effective differs from file — overlay keys win at runtime.";
  }, [view]);

  const parseBudgetRows = (rows: BudgetRow[], kind: "tenant" | "agent"): Record<string, FinOpsBudgetCap> => {
    const out: Record<string, FinOpsBudgetCap> = {};
    for (const r of rows) {
      const key = r.key.trim();
      if (!key) continue;
      if (r.usd.trim() === "") continue;
      if (kind === "agent" && !key.includes("/")) {
        throw new Error(`Agent budget key ${JSON.stringify(key)} must be tenant_id/agent_id`);
      }
      const usd = Number(r.usd);
      if (Number.isNaN(usd)) throw new Error(`Budget ${key}: max USD must be a number`);
      out[key] = { max_usd_per_day: usd, soft: r.soft };
    }
    return out;
  };

  const save = async (opts?: { confirmFree?: boolean }) => {
    setBusy(true);
    setSaveError(null);
    setSaveOk(null);
    try {
      const pricebook: Record<string, FinOpsModelPrice> = {};
      const zeroRateModels: string[] = [];
      for (const r of prices) {
        const model = r.model.trim();
        if (!model) continue;
        const input = Number(r.input);
        const output = Number(r.output);
        if (Number.isNaN(input) || Number.isNaN(output)) {
          throw new Error(`Pricebook ${model}: rates must be numbers`);
        }
        if (input === 0 && output === 0) zeroRateModels.push(model);
        pricebook[model] = { input_per_1k: input, output_per_1k: output };
      }
      if (zeroRateModels.length > 0 && !opts?.confirmFree) {
        setFreeConfirm(zeroRateModels);
        setBusy(false);
        return;
      }
      setFreeConfirm(null);
      const tenants = parseBudgetRows(tenantBudgets, "tenant");
      const agents = parseBudgetRows(agentBudgets, "agent");
      // Keep non-UI FinOps fields (routing/reservation/alerts) from the
      // previous overlay so a pricebook save does not wipe them. Those
      // knobs stay file/API-only in this UI.
      const payload = {
        ...(view?.overlay ?? {}),
        pricebook,
        budgets: {
          tenants,
          agents,
        },
      };
      const next = await api.put<AdminFinOpsView>("/finops", payload);
      setView(next);
      setSaveOk("Saved — active on this control-plane replica now; siblings catch up within ~15s.");
      await load();
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const clearOverlay = async () => {
    if (!view?.has_overlay) return;
    setClearConfirm(false);
    setBusy(true);
    setSaveError(null);
    setSaveOk(null);
    try {
      await api.del("/finops");
      setSaveOk("Overlay cleared — file baseline is effective again.");
      await load();
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const renderBudgetEditor = (
    title: string,
    hint: string,
    rows: BudgetRow[],
    setRows: (rows: BudgetRow[]) => void,
    placeholder: string,
    addLabel: string,
  ) => (
    <>
      <p className="mb-1 text-sm font-medium">{title}</p>
      <p className="mb-2 text-xs text-muted-foreground">{hint}</p>
      <div className="mb-4 space-y-2">
        {rows.map((r, i) => (
          <div key={i} className="flex flex-wrap items-center gap-2">
            <Input
              className="w-52 font-mono text-xs"
              placeholder={placeholder}
              value={r.key}
              onChange={(e) => {
                const next = [...rows];
                next[i] = { ...r, key: e.target.value };
                setRows(next);
              }}
            />
            <Input
              className="w-28 font-mono text-xs"
              placeholder="max_usd_per_day"
              value={r.usd}
              onChange={(e) => {
                const next = [...rows];
                next[i] = { ...r, usd: e.target.value };
                setRows(next);
              }}
            />
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <input
                type="checkbox"
                checked={r.soft}
                onChange={(e) => {
                  const next = [...rows];
                  next[i] = { ...r, soft: e.target.checked };
                  setRows(next);
                }}
              />
              Soft (warn, don’t deny)
            </label>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setRows(rows.filter((_, j) => j !== i))}
              disabled={rows.length <= 1}
            >
              Remove
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => setRows([...rows, { key: "", usd: "", soft: false }])}
        >
          {addLabel}
        </Button>
      </div>
    </>
  );

  if (loadError) {
    return (
      <div className="rounded-sm border border-border bg-card px-4 py-3 text-sm text-muted-foreground">
        Live FinOps editor unavailable: {loadError}
      </div>
    );
  }

  return (
    <div className="rounded-sm border border-border bg-card px-4 py-4">
      <div className="mb-3 flex flex-wrap items-start justify-between gap-2">
        <div>
          <h2 className="text-lg font-semibold">Live FinOps config</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            Edit pricebook and day budgets here — no JSON file edit, no control-plane restart. File{" "}
            <code className="text-foreground">finops</code> stays the bootstrap baseline; this overlay wins at runtime.
            Routing / reservation / hold_ttl stay file or API-only.
          </p>
        </div>
        <div className="text-xs text-muted-foreground">
          {view?.has_overlay ? (
            <span>
              Overlay active
              {view.meta?.updated_at ? ` · ${new Date(view.meta.updated_at).toLocaleString()}` : ""}
              {view.meta?.updated_by ? ` · ${view.meta.updated_by}` : ""}
            </span>
          ) : (
            <span>No overlay — using file baseline</span>
          )}
        </div>
      </div>

      {view && (
        <div className="mb-4 grid gap-2 rounded-sm border border-border bg-background/40 px-3 py-2 text-xs sm:grid-cols-2">
          <div>
            <p className="font-medium text-foreground">File baseline</p>
            <p className="mt-1 font-mono text-muted-foreground">{summarizeSide("file", view.file)}</p>
          </div>
          <div>
            <p className="font-medium text-foreground">Effective (live)</p>
            <p className="mt-1 font-mono text-muted-foreground">{summarizeSide("effective", view.effective)}</p>
          </div>
          <p className="sm:col-span-2 text-muted-foreground">{diffNote}</p>
        </div>
      )}

      <p className="mb-2 text-sm font-medium">Pricebook (USD per 1k tokens)</p>
      <div className="mb-4 space-y-2">
        {prices.map((r, i) => (
          <div key={i} className="flex flex-wrap gap-2">
            <Input
              className="w-48 font-mono text-xs"
              placeholder="model id"
              value={r.model}
              onChange={(e) => {
                const next = [...prices];
                next[i] = { ...r, model: e.target.value };
                setPrices(next);
              }}
            />
            <Input
              className="w-28 font-mono text-xs"
              placeholder="input_per_1k"
              value={r.input}
              onChange={(e) => {
                const next = [...prices];
                next[i] = { ...r, input: e.target.value };
                setPrices(next);
              }}
            />
            <Input
              className="w-28 font-mono text-xs"
              placeholder="output_per_1k"
              value={r.output}
              onChange={(e) => {
                const next = [...prices];
                next[i] = { ...r, output: e.target.value };
                setPrices(next);
              }}
            />
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setPrices(prices.filter((_, j) => j !== i))}
              disabled={prices.length <= 1}
            >
              Remove
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => setPrices([...prices, { model: "", input: "", output: "" }])}
        >
          Add model
        </Button>
      </div>

      {renderBudgetEditor(
        "Tenant day budgets (max USD)",
        "Keys are tenant_id (e.g. default).",
        tenantBudgets,
        setTenantBudgets,
        "tenant_id",
        "Add tenant budget",
      )}

      {renderBudgetEditor(
        "Agent day budgets (max USD)",
        "Keys must be tenant_id/agent_id (e.g. default/gemini_langgraph).",
        agentBudgets,
        setAgentBudgets,
        "tenant_id/agent_id",
        "Add agent budget",
      )}

      {saveError && <p className="mb-2 text-sm text-destructive">{saveError}</p>}
      {saveOk && <p className="mb-2 text-sm text-emerald-400">{saveOk}</p>}

      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" onClick={() => void save()} disabled={busy}>
          {busy ? "Saving…" : "Save live overlay"}
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => setClearConfirm(true)}
          disabled={busy || !view?.has_overlay}
        >
          Clear overlay
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => void load()} disabled={busy}>
          Reload
        </Button>
      </div>

      <Dialog open={freeConfirm != null} onOpenChange={(open) => !open && setFreeConfirm(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Save as free tier?</DialogTitle>
            <DialogDescription>
              These models have $0/$0 rates (intentional free tier): {freeConfirm?.join(", ")}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setFreeConfirm(null)} disabled={busy}>
              Cancel
            </Button>
            <Button type="button" onClick={() => void save({ confirmFree: true })} disabled={busy}>
              Save as free tier
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={clearConfirm} onOpenChange={setClearConfirm}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Clear live FinOps overlay?</DialogTitle>
            <DialogDescription>
              File langgraph.json baseline becomes effective again.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setClearConfirm(false)} disabled={busy}>
              Cancel
            </Button>
            <Button type="button" variant="destructive" onClick={() => void clearOverlay()} disabled={busy}>
              Clear overlay
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
