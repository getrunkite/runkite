import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "../api/client";
import type { AdminFinOpsView, FinOpsBudgetCap, FinOpsModelPrice } from "../api/types";
import { Button } from "../components/ui/button";
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

function budgetsToRows(tenants: Record<string, FinOpsBudgetCap> | undefined): BudgetRow[] {
  const entries = Object.entries(tenants ?? {});
  if (entries.length === 0) return [{ key: "default", usd: "", soft: false }];
  return entries.map(([key, c]) => ({
    key,
    usd: c.max_usd_per_day != null ? String(c.max_usd_per_day) : "",
    soft: Boolean(c.soft),
  }));
}

/** Guided live editor for FinOps overlays (pricebook + tenant budgets). */
export function FinOpsConfigPanel({ seedModels = [] }: { seedModels?: string[] }) {
  const [view, setView] = useState<AdminFinOpsView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveOk, setSaveOk] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [prices, setPrices] = useState<PriceRow[]>([{ model: "", input: "", output: "" }]);
  const [budgets, setBudgets] = useState<BudgetRow[]>([{ key: "default", usd: "", soft: false }]);

  const load = useCallback(async () => {
    setLoadError(null);
    try {
      const v = await api.get<AdminFinOpsView>("/finops");
      setView(v);
      const overlayPb = v.overlay?.pricebook ?? {};
      const effPb = { ...(v.file?.pricebook ?? {}), ...overlayPb };
      // Prefer editing the overlay surface: start from effective so admins
      // see what is live, then PUT as the full overlay document.
      setPrices(pricebookToRows(Object.keys(overlayPb).length > 0 ? overlayPb : effPb));
      const overlayTenants = v.overlay?.budgets?.tenants ?? {};
      const effTenants = { ...(v.file?.budgets?.tenants ?? {}), ...overlayTenants };
      setBudgets(budgetsToRows(Object.keys(overlayTenants).length > 0 ? overlayTenants : effTenants));
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
      return [...rows.filter((r) => r.model.trim() || r.input || r.output), ...add.map((m) => ({ model: m, input: "", output: "" }))];
    });
  }, [seedModels]);

  const save = async () => {
    setBusy(true);
    setSaveError(null);
    setSaveOk(null);
    try {
      const pricebook: Record<string, FinOpsModelPrice> = {};
      for (const r of prices) {
        const model = r.model.trim();
        if (!model) continue;
        const input = Number(r.input);
        const output = Number(r.output);
        if (Number.isNaN(input) || Number.isNaN(output)) {
          throw new Error(`Pricebook ${model}: rates must be numbers`);
        }
        pricebook[model] = { input_per_1k: input, output_per_1k: output };
      }
      const tenants: Record<string, FinOpsBudgetCap> = {};
      for (const r of budgets) {
        const key = r.key.trim();
        if (!key) continue;
        if (r.usd.trim() === "") continue;
        const usd = Number(r.usd);
        if (Number.isNaN(usd)) throw new Error(`Budget ${key}: max USD must be a number`);
        tenants[key] = { max_usd_per_day: usd, soft: r.soft };
      }
      const payload = {
        ...(view?.overlay ?? {}),
        pricebook,
        budgets: {
          ...(view?.overlay?.budgets ?? {}),
          tenants,
          agents: view?.overlay?.budgets?.agents,
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
    if (!window.confirm("Clear the live FinOps overlay? File langgraph.json baseline becomes effective again.")) return;
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
            Edit pricebook and tenant day budgets here — no JSON file edit, no control-plane restart.
            File <code className="text-foreground">finops</code> stays the bootstrap baseline; this overlay wins at runtime.
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
        <Button type="button" variant="outline" size="sm" onClick={() => setPrices([...prices, { model: "", input: "", output: "" }])}>
          Add model
        </Button>
      </div>

      <p className="mb-2 text-sm font-medium">Tenant day budgets (max USD)</p>
      <div className="mb-4 space-y-2">
        {budgets.map((r, i) => (
          <div key={i} className="flex flex-wrap items-center gap-2">
            <Input
              className="w-40 font-mono text-xs"
              placeholder="tenant_id"
              value={r.key}
              onChange={(e) => {
                const next = [...budgets];
                next[i] = { ...r, key: e.target.value };
                setBudgets(next);
              }}
            />
            <Input
              className="w-28 font-mono text-xs"
              placeholder="max_usd_per_day"
              value={r.usd}
              onChange={(e) => {
                const next = [...budgets];
                next[i] = { ...r, usd: e.target.value };
                setBudgets(next);
              }}
            />
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <input
                type="checkbox"
                checked={r.soft}
                onChange={(e) => {
                  const next = [...budgets];
                  next[i] = { ...r, soft: e.target.checked };
                  setBudgets(next);
                }}
              />
              Soft (warn, don’t deny)
            </label>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setBudgets(budgets.filter((_, j) => j !== i))}
              disabled={budgets.length <= 1}
            >
              Remove
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => setBudgets([...budgets, { key: "", usd: "", soft: false }])}
        >
          Add tenant budget
        </Button>
      </div>

      {saveError && <p className="mb-2 text-sm text-destructive">{saveError}</p>}
      {saveOk && <p className="mb-2 text-sm text-emerald-400">{saveOk}</p>}

      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" onClick={() => void save()} disabled={busy}>
          {busy ? "Saving…" : "Save live overlay"}
        </Button>
        <Button type="button" variant="outline" size="sm" onClick={() => void clearOverlay()} disabled={busy || !view?.has_overlay}>
          Clear overlay
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => void load()} disabled={busy}>
          Reload
        </Button>
      </div>
    </div>
  );
}
