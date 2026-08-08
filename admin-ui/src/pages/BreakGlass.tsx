import { useState } from "react";
import { Loader2, Plus, Trash2, Unlock } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminBreakGlassWindow } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
import { ListPager, adminListPath } from "../components/list-pager";
import { Badge } from "../components/ui/badge";
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
import { Label } from "../components/ui/label";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

interface BreakGlassForm {
  id: string;
  tenant_id: string;
  agent_id: string;
  reason: string;
  expires_at: string; // datetime-local value
}

const emptyForm = (): BreakGlassForm => {
  const expires = new Date(Date.now() + 60 * 60 * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  const local = `${expires.getFullYear()}-${pad(expires.getMonth() + 1)}-${pad(expires.getDate())}T${pad(expires.getHours())}:${pad(expires.getMinutes())}`;
  return {
    id: "",
    tenant_id: "",
    agent_id: "",
    reason: "",
    expires_at: local,
  };
};

function toRFC3339(local: string): string {
  const d = new Date(local);
  if (Number.isNaN(d.getTime())) return "";
  return d.toISOString();
}

export function BreakGlass() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;

  const [dialogOpen, setDialogOpen] = useState(false);
  const [form, setForm] = useState<BreakGlassForm>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<AdminBreakGlassWindow | null>(null);
  const [revoking, setRevoking] = useState(false);

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const agent = agentId.trim();
  if (tenant) extra.tenant_id = tenant;
  if (agent) extra.agent_id = agent;

  const path = adminListPath("/break-glass", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor, reload } = useApi<AdminBreakGlassWindow[]>(path, [tenant, agent, cursor]);
  const onFirstPage = cursorStack.length === 0;
  const hasFilter = tenant !== "" || agent !== "";

  const mint = async () => {
    const tenant_id = form.tenant_id.trim();
    const reason = form.reason.trim();
    const expires_at = toRFC3339(form.expires_at);
    if (!tenant_id) {
      toast.error("Missing tenant", { description: "tenant_id is required." });
      return;
    }
    if (!reason) {
      toast.error("Missing reason", { description: "reason is required for audit." });
      return;
    }
    if (!expires_at) {
      toast.error("Invalid expiry", { description: "expires_at must be a valid datetime." });
      return;
    }
    setSaving(true);
    try {
      const payload: Record<string, unknown> = { tenant_id, reason, expires_at };
      const agent_id = form.agent_id.trim();
      if (agent_id) payload.agent_id = agent_id;
      const id = form.id.trim();
      if (id) payload.id = id;

      const created = await api.post<AdminBreakGlassWindow>("/break-glass", payload);
      const scope = agent_id ? `${tenant_id}/${agent_id}` : `${tenant_id} (whole tenant)`;
      toast.success("Break-glass minted", {
        description: `${scope} — policy bypass until ${created.expires_at}. Kill/authz/limits still apply.`,
      });
      setDialogOpen(false);
      setCursorStack([]);
      reload();
    } catch (err) {
      toast.error("Mint failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setSaving(false);
    }
  };

  const revoke = async () => {
    if (!revokeTarget) return;
    setRevoking(true);
    try {
      await api.del(`/break-glass/${encodeURIComponent(revokeTarget.id)}`);
      toast.success("Break-glass revoked", { description: revokeTarget.id });
      setRevokeTarget(null);
      reload();
    } catch (err) {
      toast.error("Revoke failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setRevoking(false);
    }
  };

  const columns: ColumnDef<AdminBreakGlassWindow, unknown>[] = [
    {
      accessorKey: "tenant_id",
      header: "Tenant",
      cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
    },
    {
      accessorKey: "agent_id",
      header: "Agent",
      cell: ({ getValue }) => {
        const v = getValue() as string | undefined;
        if (!v) return <span className="text-xs text-muted-foreground">all agents</span>;
        return <span className="font-mono text-xs">{v}</span>;
      },
    },
    {
      accessorKey: "reason",
      header: "Reason",
      cell: ({ getValue }) => {
        const v = (getValue() as string | undefined) || "";
        if (!v) return <span className="text-muted-foreground">—</span>;
        return (
          <span className="max-w-[14rem] truncate text-xs text-muted-foreground" title={v}>
            {v}
          </span>
        );
      },
    },
    {
      accessorKey: "expires_at",
      header: "Expires",
      cell: ({ getValue }) => {
        const iso = getValue() as string | undefined;
        if (!iso) return <span className="text-muted-foreground">—</span>;
        const past = new Date(iso).getTime() <= Date.now();
        return (
          <Tooltip>
            <TooltipTrigger className={past ? "text-destructive" : "text-muted-foreground"}>
              {past ? "expired · " : ""}
              {formatRelativeTime(iso)}
            </TooltipTrigger>
            <TooltipContent>{formatTimestamp(iso)}</TooltipContent>
          </Tooltip>
        );
      },
    },
    {
      accessorKey: "id",
      header: "ID",
      cell: ({ getValue }) => <span className="font-mono text-xs text-muted-foreground">{getValue() as string}</span>,
    },
    {
      accessorKey: "created_by",
      header: "By",
      cell: ({ getValue }) => {
        const v = getValue() as string | undefined;
        if (!v) return <span className="text-muted-foreground">—</span>;
        return <span className="text-xs text-muted-foreground">{v}</span>;
      },
    },
    {
      id: "actions",
      header: "",
      enableSorting: false,
      cell: ({ row }) => (
        <div className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => setRevokeTarget(row.original)}>
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        title="Break-glass"
        subtitle="Time-bounded policy bypass (max 24h). Does not override kill, agent authz, or admission limits. SQL backends only."
        actions={
          <Button size="sm" onClick={() => { setForm(emptyForm()); setDialogOpen(true); }}>
            <Plus className="size-3.5" />
            Mint window
          </Button>
        }
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Input
          className="w-40"
          placeholder="Tenant ID"
          value={tenantId}
          onChange={(e) => {
            setTenantId(e.target.value);
            setCursorStack([]);
          }}
        />
        <Input
          className="w-40"
          placeholder="Agent ID"
          value={agentId}
          onChange={(e) => {
            setAgentId(e.target.value);
            setCursorStack([]);
          }}
        />
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && onFirstPage && (
        <EmptyState
          icon={Unlock}
          title={hasFilter ? "No matching windows" : "No break-glass windows"}
          message={
            hasFilter
              ? "No windows match these filters."
              : "Mint a window to temporarily bypass policy Decide for run.create / connector.session / tool.call. Requires a SQL state backend."
          }
          action={
            hasFilter ? (
              <button
                type="button"
                className="text-sm font-medium text-primary hover:underline"
                onClick={() => {
                  setTenantId("");
                  setAgentId("");
                  setCursorStack([]);
                }}
              >
                Clear filters
              </button>
            ) : (
              <Button size="sm" onClick={() => { setForm(emptyForm()); setDialogOpen(true); }}>
                <Plus className="size-3.5" />
                Mint window
              </Button>
            )
          }
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable columns={columns} data={data ?? []} getRowId={(w) => w.id} loading={loading} />
          <ListPager
            pageIndex={cursorStack.length}
            pageCount={data?.length ?? 0}
            hasNext={Boolean(nextCursor)}
            onPrev={() => setCursorStack((s) => s.slice(0, -1))}
            onNext={() => {
              if (nextCursor) setCursorStack((s) => [...s, nextCursor]);
            }}
          />
        </>
      )}

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Mint break-glass window</DialogTitle>
            <DialogDescription>
              Bypasses policy Decide only (max 24h). Kill switches, agent-scoped run authz, and admission_limits still
              apply. Each use writes a durable audit row.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="bg-tenant">Tenant ID</Label>
              <Input
                id="bg-tenant"
                value={form.tenant_id}
                onChange={(e) => setForm((f) => ({ ...f, tenant_id: e.target.value }))}
                placeholder="acme"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="bg-agent">Agent ID (optional)</Label>
              <Input
                id="bg-agent"
                value={form.agent_id}
                onChange={(e) => setForm((f) => ({ ...f, agent_id: e.target.value }))}
                placeholder="empty = whole tenant"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="bg-reason">Reason</Label>
              <Input
                id="bg-reason"
                value={form.reason}
                onChange={(e) => setForm((f) => ({ ...f, reason: e.target.value }))}
                placeholder="sev-1 customer unblock"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="bg-expires">Expires at</Label>
              <Input
                id="bg-expires"
                type="datetime-local"
                value={form.expires_at}
                onChange={(e) => setForm((f) => ({ ...f, expires_at: e.target.value }))}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="bg-id">ID (optional)</Label>
              <Input
                id="bg-id"
                value={form.id}
                onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                placeholder="auto: bg-<hex>"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={saving}>
              Cancel
            </Button>
            <Button onClick={mint} disabled={saving}>
              {saving ? <Loader2 className="size-3.5 animate-spin" /> : null}
              Mint window
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={revokeTarget !== null}
        onOpenChange={(open) => {
          if (!open && !revoking) setRevokeTarget(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke this window?</DialogTitle>
            <DialogDescription>
              Removes{" "}
              <code className="font-mono">{revokeTarget?.id}</code>
              {revokeTarget?.agent_id
                ? ` for ${revokeTarget.tenant_id}/${revokeTarget.agent_id}`
                : revokeTarget
                  ? ` for tenant ${revokeTarget.tenant_id} (all agents)`
                  : ""}
              . Policy Decide applies again immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRevokeTarget(null)} disabled={revoking}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={revoke} disabled={revoking}>
              {revoking && <Loader2 className="size-3.5 animate-spin" />}
              Revoke
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
