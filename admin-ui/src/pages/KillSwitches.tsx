import { useState } from "react";
import { Ban, Loader2, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminKillSwitch, AdminKillSwitchCreateResponse } from "../api/types";
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

interface KillForm {
  id: string;
  tenant_id: string;
  agent_id: string;
  pause_only: boolean;
  reason: string;
}

const emptyForm = (): KillForm => ({
  id: "",
  tenant_id: "",
  agent_id: "",
  pause_only: false,
  reason: "",
});

export function KillSwitches() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;

  const [dialogOpen, setDialogOpen] = useState(false);
  const [form, setForm] = useState<KillForm>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [clearTarget, setClearTarget] = useState<AdminKillSwitch | null>(null);
  const [clearing, setClearing] = useState(false);

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const agent = agentId.trim();
  if (tenant) extra.tenant_id = tenant;
  if (agent) extra.agent_id = agent;

  const path = adminListPath("/kill-switches", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor, reload } = useApi<AdminKillSwitch[]>(path, [tenant, agent, cursor]);
  const onFirstPage = cursorStack.length === 0;
  const hasFilter = tenant !== "" || agent !== "";

  const activate = async () => {
    const tenant_id = form.tenant_id.trim();
    if (!tenant_id) {
      toast.error("Missing tenant", { description: "tenant_id is required." });
      return;
    }
    setSaving(true);
    try {
      const payload: Record<string, unknown> = {
        tenant_id,
        pause_only: form.pause_only,
      };
      const agent_id = form.agent_id.trim();
      if (agent_id) payload.agent_id = agent_id;
      const id = form.id.trim();
      if (id) payload.id = id;
      const reason = form.reason.trim();
      if (reason) payload.reason = reason;

      const created = await api.post<AdminKillSwitchCreateResponse>("/kill-switches", payload);
      const scope = agent_id ? `${tenant_id}/${agent_id}` : `${tenant_id} (whole tenant)`;
      if (form.pause_only) {
        toast.success("Pause activated", {
          description: `${scope} — new runs blocked; in-flight runs left alone.`,
        });
      } else {
        toast.success("Kill activated", {
          description: `${scope} — cancelled ${created.cancelled ?? 0} pending/running run(s).`,
        });
      }
      setDialogOpen(false);
      setCursorStack([]);
      reload();
    } catch (err) {
      toast.error("Activate failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setSaving(false);
    }
  };

  const clear = async () => {
    if (!clearTarget) return;
    setClearing(true);
    try {
      await api.del(`/kill-switches/${encodeURIComponent(clearTarget.id)}`);
      toast.success("Kill switch cleared", { description: clearTarget.id });
      setClearTarget(null);
      reload();
    } catch (err) {
      toast.error("Clear failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setClearing(false);
    }
  };

  const columns: ColumnDef<AdminKillSwitch, unknown>[] = [
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
      id: "mode",
      header: "Mode",
      enableSorting: false,
      cell: ({ row }) =>
        row.original.pause_only ? (
          <Badge variant="secondary">pause</Badge>
        ) : (
          <Badge variant="destructive">kill + drain</Badge>
        ),
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
      accessorKey: "updated_at",
      header: "Updated",
      cell: ({ getValue }) => {
        const iso = getValue() as string | undefined;
        if (!iso) return <span className="text-muted-foreground">—</span>;
        return (
          <Tooltip>
            <TooltipTrigger className="text-muted-foreground">{formatRelativeTime(iso)}</TooltipTrigger>
            <TooltipContent>{formatTimestamp(iso)}</TooltipContent>
          </Tooltip>
        );
      },
    },
    {
      id: "actions",
      header: "",
      enableSorting: false,
      cell: ({ row }) => (
        <div className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" onClick={() => setClearTarget(row.original)}>
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        title="Kill switches"
        subtitle="Block new runs for a tenant or tenant+agent. Kill also drains pending/running runs on this replica. SQL backends only."
        actions={
          <Button size="sm" variant="destructive" onClick={() => { setForm(emptyForm()); setDialogOpen(true); }}>
            <Plus className="size-3.5" />
            Activate
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
          icon={Ban}
          title={hasFilter ? "No matching kill switches" : "No active kill switches"}
          message={
            hasFilter
              ? "No flags match these filters."
              : "Activate a kill or pause to refuse new creates (and optionally drain in-flight runs). Requires a SQL state backend."
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
              <Button size="sm" variant="destructive" onClick={() => { setForm(emptyForm()); setDialogOpen(true); }}>
                <Plus className="size-3.5" />
                Activate
              </Button>
            )
          }
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable columns={columns} data={data ?? []} getRowId={(k) => k.id} loading={loading} />
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
            <DialogTitle>Activate kill switch</DialogTitle>
            <DialogDescription>
              New run creates in scope fail closed immediately on every replica (SQL read). Unless pause-only, this
              replica also cancels all pending and running runs in scope.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="kill-tenant">Tenant ID</Label>
              <Input
                id="kill-tenant"
                value={form.tenant_id}
                onChange={(e) => setForm((f) => ({ ...f, tenant_id: e.target.value }))}
                placeholder="acme"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="kill-agent">Agent ID (optional)</Label>
              <Input
                id="kill-agent"
                value={form.agent_id}
                onChange={(e) => setForm((f) => ({ ...f, agent_id: e.target.value }))}
                placeholder="empty = whole tenant"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="kill-reason">Reason (optional)</Label>
              <Input
                id="kill-reason"
                value={form.reason}
                onChange={(e) => setForm((f) => ({ ...f, reason: e.target.value }))}
                placeholder="incident response"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="kill-id">ID (optional)</Label>
              <Input
                id="kill-id"
                value={form.id}
                onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                placeholder="auto: kill-<tenant>[-<agent>]"
              />
            </div>
            <label className="flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                className="mt-1"
                checked={form.pause_only}
                onChange={(e) => setForm((f) => ({ ...f, pause_only: e.target.checked }))}
              />
              <span>
                <span className="font-medium">Pause only</span>
                <span className="block text-muted-foreground">
                  Block new creates; do not cancel in-flight runs.
                </span>
              </span>
            </label>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={saving}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={activate} disabled={saving}>
              {saving ? <Loader2 className="size-3.5 animate-spin" /> : null}
              {form.pause_only ? "Activate pause" : "Activate kill + drain"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={clearTarget !== null}
        onOpenChange={(open) => {
          if (!open && !clearing) setClearTarget(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Clear this kill switch?</DialogTitle>
            <DialogDescription>
              Removes{" "}
              <code className="font-mono">{clearTarget?.id}</code>
              {clearTarget?.agent_id
                ? ` for ${clearTarget.tenant_id}/${clearTarget.agent_id}`
                : clearTarget
                  ? ` for tenant ${clearTarget.tenant_id} (all agents)`
                  : ""}
              . New run creates in that scope are allowed again (subject to authz and policy).
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setClearTarget(null)} disabled={clearing}>
              Cancel
            </Button>
            <Button onClick={clear} disabled={clearing}>
              {clearing && <Loader2 className="size-3.5 animate-spin" />}
              Clear switch
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
