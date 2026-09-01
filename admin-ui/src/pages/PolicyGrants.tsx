import { useState } from "react";
import { KeyRound, Loader2, Pencil, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminPolicyGrant } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, supportPage } from "../components/common";
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

function toolsSummary(g: AdminPolicyGrant): string {
  const allow = g.tools?.allow ?? [];
  const deny = g.tools?.deny ?? [];
  if (allow.length === 0 && deny.length === 0) return "all tools";
  const parts: string[] = [];
  if (allow.length) parts.push(`allow: ${allow.join(", ")}`);
  if (deny.length) parts.push(`deny: ${deny.join(", ")}`);
  return parts.join("; ");
}

function parseToolList(raw: string): string[] {
  return raw
    .split(/[,\s]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

interface GrantForm {
  id: string;
  tenant_id: string;
  agent_id: string;
  connector: string;
  allow: string;
  deny: string;
}

const emptyForm = (): GrantForm => ({
  id: "",
  tenant_id: "",
  agent_id: "",
  connector: "",
  allow: "",
  deny: "",
});

function formFromGrant(g: AdminPolicyGrant): GrantForm {
  return {
    id: g.id,
    tenant_id: g.tenant_id,
    agent_id: g.agent_id,
    connector: g.connector,
    allow: (g.tools?.allow ?? []).join(", "),
    deny: (g.tools?.deny ?? []).join(", "),
  };
}

function bodyFromForm(f: GrantForm): AdminPolicyGrant {
  const allow = parseToolList(f.allow);
  const deny = parseToolList(f.deny);
  const out: AdminPolicyGrant = {
    id: f.id.trim(),
    tenant_id: f.tenant_id.trim(),
    agent_id: f.agent_id.trim(),
    connector: f.connector.trim(),
  };
  if (allow.length || deny.length) {
    out.tools = {};
    if (allow.length) out.tools.allow = allow;
    if (deny.length) out.tools.deny = deny;
  }
  return out;
}

export function PolicyGrants() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const [connector, setConnector] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState<GrantForm>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<AdminPolicyGrant | null>(null);
  const [deleting, setDeleting] = useState(false);

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const agent = agentId.trim();
  const conn = connector.trim();
  if (tenant) extra.tenant_id = tenant;
  if (agent) extra.agent_id = agent;
  if (conn) extra.connector = conn;

  const path = adminListPath("/policy-grants", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor, reload } = useApi<AdminPolicyGrant[]>(path, [
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

  const openEdit = (g: AdminPolicyGrant) => {
    setEditing(true);
    setForm(formFromGrant(g));
    setDialogOpen(true);
  };

  const save = async () => {
    const body = bodyFromForm(form);
    if (!body.tenant_id || !body.agent_id || !body.connector) {
      toast.error("Missing fields", { description: "tenant_id, agent_id, and connector are required." });
      return;
    }
    setSaving(true);
    try {
      if (editing) {
        await api.put(`/policy-grants/${encodeURIComponent(body.id)}`, body);
        toast.success("Grant updated", { description: body.id });
      } else {
        const payload: Record<string, unknown> = {
          tenant_id: body.tenant_id,
          agent_id: body.agent_id,
          connector: body.connector,
        };
        if (body.id) payload.id = body.id;
        if (body.tools) payload.tools = body.tools;
        const created = await api.post<AdminPolicyGrant>("/policy-grants", payload);
        toast.success("Grant created", { description: created.id });
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
      await api.del(`/policy-grants/${encodeURIComponent(deleteTarget.id)}`);
      toast.success("Grant deleted", { description: deleteTarget.id });
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

  const columns: ColumnDef<AdminPolicyGrant, unknown>[] = [
    {
      accessorKey: "tenant_id",
      header: "Tenant",
      cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
    },
    {
      accessorKey: "agent_id",
      header: "Agent",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "connector",
      header: "Connector",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      id: "tools",
      header: "Tools",
      enableSorting: false,
      cell: ({ row }) => (
        <span className="max-w-[16rem] truncate text-xs text-muted-foreground" title={toolsSummary(row.original)}>
          {toolsSummary(row.original)}
        </span>
      ),
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
        title="Policy grants"
        subtitle="Durable connector overlays (SQL backends). DB rows win over langgraph.json on the same tenant/agent/connector."
        actions={
          <Button size="sm" onClick={openCreate}>
            <Plus className="size-3.5" />
            New grant
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
          icon={KeyRound}
          title={hasFilter ? "No matching grants" : "No durable grants yet"}
          message={
            hasFilter
              ? "No overlays match these filters."
              : "Create a grant here or seed policy.grants in langgraph.json. Requires a SQL state backend and an enabled policy section."
          }
          learnMore={
            hasFilter
              ? undefined
              : { href: supportPage("connectors.html"), label: "Docs: connectors & grants →" }
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
                New grant
              </Button>
            )
          }
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable columns={columns} data={data ?? []} getRowId={(g) => g.id} loading={loading} />
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
            <DialogTitle>{editing ? "Edit grant" : "New grant"}</DialogTitle>
            <DialogDescription>
              Overlays hot-reload on this replica. Leave tool lists empty to allow every tool on the connector.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            {!editing && (
              <div className="grid gap-1.5">
                <Label htmlFor="grant-id">ID (optional)</Label>
                <Input
                  id="grant-id"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  placeholder="auto-allocated if empty"
                />
              </div>
            )}
            <div className="grid gap-1.5">
              <Label htmlFor="grant-tenant">Tenant ID</Label>
              <Input
                id="grant-tenant"
                value={form.tenant_id}
                onChange={(e) => setForm((f) => ({ ...f, tenant_id: e.target.value }))}
                disabled={editing}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="grant-agent">Agent ID</Label>
              <Input
                id="grant-agent"
                value={form.agent_id}
                onChange={(e) => setForm((f) => ({ ...f, agent_id: e.target.value }))}
                disabled={editing}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="grant-connector">Connector</Label>
              <Input
                id="grant-connector"
                value={form.connector}
                onChange={(e) => setForm((f) => ({ ...f, connector: e.target.value }))}
                disabled={editing}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="grant-allow">Allow tools (comma-separated)</Label>
              <Input
                id="grant-allow"
                value={form.allow}
                onChange={(e) => setForm((f) => ({ ...f, allow: e.target.value }))}
                placeholder="query, getRecord"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="grant-deny">Deny tools (comma-separated)</Label>
              <Input
                id="grant-deny"
                value={form.deny}
                onChange={(e) => setForm((f) => ({ ...f, deny: e.target.value }))}
                placeholder="updateRecord"
              />
            </div>
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
            <DialogTitle>Delete this grant?</DialogTitle>
            <DialogDescription>
              This removes the durable overlay{" "}
              <code className="font-mono">{deleteTarget?.id}</code> for{" "}
              <code className="font-mono">
                {deleteTarget?.tenant_id}/{deleteTarget?.agent_id}/{deleteTarget?.connector}
              </code>
              . The in-process engine on this replica reloads immediately. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)} disabled={deleting}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={remove} disabled={deleting}>
              {deleting && <Loader2 className="size-3.5 animate-spin" />}
              Delete grant
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
