import { useState } from "react";
import { Loader2, Pencil, Plus, ShieldCheck, Trash2 } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminMandatoryHITLRule } from "../api/types";
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

function parseToolList(raw: string): string[] {
  return raw
    .split(/[,\s]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

interface Form {
  id: string;
  tenant_id: string;
  agent_id: string;
  connector: string;
  tools: string;
}

const emptyForm = (): Form => ({
  id: "",
  tenant_id: "",
  agent_id: "",
  connector: "",
  tools: "",
});

function formFromRule(r: AdminMandatoryHITLRule): Form {
  return {
    id: r.id,
    tenant_id: r.tenant_id,
    agent_id: r.agent_id ?? "",
    connector: r.connector,
    tools: (r.tools ?? []).join(", "),
  };
}

export function MandatoryHITL() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const [connector, setConnector] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState<Form>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<AdminMandatoryHITLRule | null>(null);
  const [deleting, setDeleting] = useState(false);

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const agent = agentId.trim();
  const conn = connector.trim();
  if (tenant) extra.tenant_id = tenant;
  if (agent) extra.agent_id = agent;
  if (conn) extra.connector = conn;

  const path = adminListPath("/mandatory-hitl", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor, reload } = useApi<AdminMandatoryHITLRule[]>(path, [
    tenant,
    agent,
    conn,
    cursor,
  ]);
  const onFirstPage = cursorStack.length === 0;
  const hasFilter = tenant !== "" || agent !== "" || conn !== "";

  const openCreate = () => {
    setEditing(false);
    setForm(emptyForm());
    setDialogOpen(true);
  };

  const openEdit = (r: AdminMandatoryHITLRule) => {
    setEditing(true);
    setForm(formFromRule(r));
    setDialogOpen(true);
  };

  const save = async () => {
    const tenant_id = form.tenant_id.trim();
    const connectorName = form.connector.trim();
    if (!tenant_id || !connectorName) {
      toast.error("Missing fields", { description: "tenant_id and connector are required." });
      return;
    }
    setSaving(true);
    try {
      const payload: Record<string, unknown> = {
        tenant_id,
        connector: connectorName,
      };
      const agent_id = form.agent_id.trim();
      if (agent_id) payload.agent_id = agent_id;
      const tools = parseToolList(form.tools);
      if (tools.length) payload.tools = tools;
      const id = form.id.trim();
      if (id) payload.id = id;

      if (editing) {
        await api.put(`/mandatory-hitl/${encodeURIComponent(id)}`, { ...payload, id });
        toast.success("Rule updated");
      } else {
        const created = await api.post<AdminMandatoryHITLRule>("/mandatory-hitl", payload);
        toast.success("Rule created", { description: created.id });
      }
      setDialogOpen(false);
      setCursorStack([]);
      reload();
    } catch (err) {
      toast.error(editing ? "Update failed" : "Create failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await api.del(`/mandatory-hitl/${encodeURIComponent(deleteTarget.id)}`);
      toast.success("Rule deleted", { description: deleteTarget.id });
      setDeleteTarget(null);
      reload();
    } catch (err) {
      toast.error("Delete failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setDeleting(false);
    }
  };

  const columns: ColumnDef<AdminMandatoryHITLRule, unknown>[] = [
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
      accessorKey: "connector",
      header: "Connector",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "tools",
      header: "Tools",
      cell: ({ row }) => {
        const tools = row.original.tools ?? [];
        if (tools.length === 0) return <span className="text-xs text-muted-foreground">all tools</span>;
        return (
          <span className="max-w-[14rem] truncate text-xs text-muted-foreground" title={tools.join(", ")}>
            {tools.join(", ")}
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
          <Button variant="ghost" size="sm" onClick={() => openEdit(row.original)}>
            <Pencil className="size-3.5" />
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setDeleteTarget(row.original)}>
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        title="Mandatory HITL"
        subtitle="Force matching tool.call allows to pending (Admin approve → one-shot retry). Config baselines + SQL overlays. Hard deny still wins. SQL backends only."
        actions={
          <Button size="sm" onClick={openCreate}>
            <Plus className="size-3.5" />
            Add rule
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
        <Input
          className="w-40"
          placeholder="Connector"
          value={connector}
          onChange={(e) => {
            setConnector(e.target.value);
            setCursorStack([]);
          }}
        />
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && onFirstPage && (
        <EmptyState
          icon={ShieldCheck}
          title={hasFilter ? "No matching rules" : "No mandatory HITL overlays"}
          message={
            hasFilter
              ? "No SQL overlays match these filters. Config baselines in langgraph.json are not listed here."
              : "Add a durable overlay, or set policy.mandatory_hitl in langgraph.json (config baselines are not listed here)."
          }
          action={
            hasFilter ? (
              <button
                type="button"
                className="text-sm font-medium text-primary hover:underline"
                onClick={() => {
                  setTenantId("");
                  setAgentId("");
                  setConnector("");
                  setCursorStack([]);
                }}
              >
                Clear filters
              </button>
            ) : (
              <Button size="sm" onClick={openCreate}>
                <Plus className="size-3.5" />
                Add rule
              </Button>
            )
          }
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable columns={columns} data={data ?? []} getRowId={(r) => r.id} loading={loading} />
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
            <DialogTitle>{editing ? "Edit mandatory HITL rule" : "Add mandatory HITL rule"}</DialogTitle>
            <DialogDescription>
              Forces matching tool.call allows to pending even when grants/webhook would allow. Empty agent = whole
              tenant; empty tools = all tools on the connector.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="mh-tenant">Tenant ID</Label>
              <Input
                id="mh-tenant"
                value={form.tenant_id}
                onChange={(e) => setForm((f) => ({ ...f, tenant_id: e.target.value }))}
                placeholder="acme"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="mh-agent">Agent ID (optional)</Label>
              <Input
                id="mh-agent"
                value={form.agent_id}
                onChange={(e) => setForm((f) => ({ ...f, agent_id: e.target.value }))}
                placeholder="empty = whole tenant"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="mh-connector">Connector</Label>
              <Input
                id="mh-connector"
                value={form.connector}
                onChange={(e) => setForm((f) => ({ ...f, connector: e.target.value }))}
                placeholder="github"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="mh-tools">Tools (comma-separated, optional)</Label>
              <Input
                id="mh-tools"
                value={form.tools}
                onChange={(e) => setForm((f) => ({ ...f, tools: e.target.value }))}
                placeholder="delete_repo, transfer_funds"
              />
            </div>
            {!editing && (
              <div className="grid gap-1.5">
                <Label htmlFor="mh-id">ID (optional)</Label>
                <Input
                  id="mh-id"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  placeholder="auto: mhitl-<hex>"
                />
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={saving}>
              Cancel
            </Button>
            <Button onClick={save} disabled={saving}>
              {saving ? <Loader2 className="size-3.5 animate-spin" /> : null}
              {editing ? "Save" : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open && !deleting) setDeleteTarget(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete this rule?</DialogTitle>
            <DialogDescription>
              Removes SQL overlay <code className="font-mono">{deleteTarget?.id}</code>. Config baselines in
              langgraph.json are unaffected.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)} disabled={deleting}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={remove} disabled={deleting}>
              {deleting && <Loader2 className="size-3.5 animate-spin" />}
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
