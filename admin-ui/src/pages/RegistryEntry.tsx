import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { Loader2, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminRegistryEntry, AdminRegistryEntryVersion } from "../api/types";
import { ErrorState, formatTimestamp, PageHeader, supportPage } from "../components/common";
import { adminRegistryPath, adminRegistryVersionsPath, adminTenantQuery } from "../lib/adminPaths";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
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
import { Skeleton } from "../components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";

interface EntryForm {
  tenant_id: string;
  name: string;
  display_name: string;
  description: string;
  author: string;
  tags: string;
  source_type: string;
  source_ref: string;
}

const emptyForm = (): EntryForm => ({
  tenant_id: "default",
  name: "",
  display_name: "",
  description: "",
  author: "",
  tags: "",
  source_type: "git",
  source_ref: "",
});

function formFromEntry(e: AdminRegistryEntry): EntryForm {
  return {
    tenant_id: e.tenant_id,
    name: e.name,
    display_name: e.display_name ?? "",
    description: e.description ?? "",
    author: e.author ?? "",
    tags: (e.tags ?? []).join(", "),
    source_type: e.source_type,
    source_ref: e.source_ref,
  };
}

function bodyFromForm(f: EntryForm): Omit<AdminRegistryEntry, "tenant_id" | "version" | "created_at" | "updated_at"> {
  const tags = f.tags
    .split(/[,\s]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  return {
    name: f.name.trim(),
    display_name: f.display_name.trim() || undefined,
    description: f.description.trim() || undefined,
    author: f.author.trim() || undefined,
    tags: tags.length ? tags : undefined,
    source_type: f.source_type.trim(),
    source_ref: f.source_ref.trim(),
  };
}

export function RegistryEntry() {
  const { name: routeName } = useParams<{ name: string }>();
  const isNew = routeName === "new";
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const initialTenant = searchParams.get("tenant_id") ?? "default";

  const [form, setForm] = useState<EntryForm>(() => ({ ...emptyForm(), tenant_id: initialTenant }));
  const [saving, setSaving] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const entryPath = isNew || !routeName ? null : adminRegistryPath(routeName, form.tenant_id);
  const versionsPath = isNew || !routeName ? null : adminRegistryVersionsPath(routeName, form.tenant_id);

  const entry = useApi<AdminRegistryEntry>(entryPath ?? "", [routeName, form.tenant_id]);
  const versions = useApi<AdminRegistryEntryVersion[]>(versionsPath ?? "", [routeName, form.tenant_id]);

  useEffect(() => {
    if (entry.data && !isNew) setForm(formFromEntry(entry.data));
  }, [entry.data, isNew]);

  const handleSave = async () => {
    const slug = form.name.trim();
    if (!slug) {
      toast.error("Name is required", { description: "Use a URL-safe slug, e.g. sales-qualifier." });
      return;
    }
    if (!form.source_type.trim() || !form.source_ref.trim()) {
      toast.error("Source is required", { description: "Both source_type and source_ref must be set." });
      return;
    }
    setSaving(true);
    try {
      const q = adminTenantQuery(form.tenant_id);
      const saved = await api.put<AdminRegistryEntry>(`/registry/${encodeURIComponent(slug)}${q}`, bodyFromForm(form));
      toast.success(isNew ? "Entry published" : "Entry updated", { description: slug });
      if (isNew) {
        navigate(`/admin/registry/${encodeURIComponent(slug)}?tenant_id=${encodeURIComponent(saved.tenant_id)}`, {
          replace: true,
        });
      } else {
        entry.reload();
        versions.reload();
      }
    } catch (err) {
      toast.error("Save failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!routeName || isNew) return;
    setDeleting(true);
    try {
      const q = adminTenantQuery(form.tenant_id);
      await api.del(`/registry/${encodeURIComponent(routeName)}${q}`);
      toast.success("Entry deleted", { description: routeName });
      navigate("/admin/registry");
    } catch (err) {
      toast.error("Delete failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
      setDeleting(false);
      setDeleteOpen(false);
    }
  };

  if (!isNew && entry.loading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }
  if (!isNew && entry.error) return <ErrorState message={entry.error} />;

  return (
    <div>
      <PageHeader
        title={isNew ? "Publish registry entry" : form.name || routeName || "Registry entry"}
        subtitle={
          isNew
            ? "Metadata catalog only — publishing does not deploy runnable code."
            : entry.data
              ? `v${entry.data.version} · updated ${formatTimestamp(entry.data.updated_at)}`
              : undefined
        }
        actions={
          <a
            href={supportPage("registry.html")}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-primary hover:underline"
          >
            Docs: registry →
          </a>
        }
      />

      <div className="mb-4">
        <Button variant="outline" size="sm" asChild>
          <Link to="/admin/registry">← All entries</Link>
        </Button>
      </div>

      <Card className="mb-4">
        <CardHeader>
          <CardTitle className="text-base">{isNew ? "New entry" : "Edit entry"}</CardTitle>
        </CardHeader>
        <CardContent className="grid max-w-2xl gap-4">
          <div className="grid gap-2">
            <Label htmlFor="tenant_id">Tenant</Label>
            <Input
              id="tenant_id"
              value={form.tenant_id}
              onChange={(e) => setForm((f) => ({ ...f, tenant_id: e.target.value }))}
              disabled={!isNew}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="name">Name (slug)</Label>
            <Input
              id="name"
              value={form.name}
              onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
              disabled={!isNew}
              placeholder="sales-qualifier"
              className="font-mono text-sm"
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="display_name">Display name</Label>
            <Input
              id="display_name"
              value={form.display_name}
              onChange={(e) => setForm((f) => ({ ...f, display_name: e.target.value }))}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="description">Description</Label>
            <Input
              id="description"
              value={form.description}
              onChange={(e) => setForm((f) => ({ ...f, description: e.target.value }))}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="author">Author</Label>
            <Input id="author" value={form.author} onChange={(e) => setForm((f) => ({ ...f, author: e.target.value }))} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tags">Tags (comma-separated)</Label>
            <Input id="tags" value={form.tags} onChange={(e) => setForm((f) => ({ ...f, tags: e.target.value }))} />
          </div>
          <div className="grid gap-2 sm:grid-cols-2 sm:gap-4">
            <div className="grid gap-2">
              <Label htmlFor="source_type">Source type</Label>
              <Input
                id="source_type"
                value={form.source_type}
                onChange={(e) => setForm((f) => ({ ...f, source_type: e.target.value }))}
                placeholder="git | url | inline"
                className="font-mono text-sm"
              />
            </div>
            <div className="grid gap-2 sm:col-span-1">
              <Label htmlFor="source_ref">Source ref</Label>
              <Input
                id="source_ref"
                value={form.source_ref}
                onChange={(e) => setForm((f) => ({ ...f, source_ref: e.target.value }))}
                className="font-mono text-sm"
              />
            </div>
          </div>
          <div className="flex flex-wrap gap-2 pt-2">
            <Button onClick={handleSave} disabled={saving}>
              {saving && <Loader2 className="size-3.5 animate-spin" />}
              {isNew ? "Publish" : "Save changes"}
            </Button>
            {!isNew && (
              <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
                <Trash2 className="size-3.5" />
                Delete
              </Button>
            )}
          </div>
          <p className="text-xs text-muted-foreground">
            Registry entries are discoverable metadata. Wiring <code className="text-foreground">source_ref</code> into a
            runner still requires deploy/restart (Phase C.2).
          </p>
        </CardContent>
      </Card>

      {!isNew && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Version history</CardTitle>
          </CardHeader>
          <CardContent>
            {versions.error ? (
              <p className="text-sm text-muted-foreground">{versions.error}</p>
            ) : versions.loading ? (
              <Skeleton className="h-24 w-full" />
            ) : !versions.data?.length ? (
              <p className="text-sm text-muted-foreground">No version snapshots yet.</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Version</TableHead>
                    <TableHead>Display name</TableHead>
                    <TableHead>Source</TableHead>
                    <TableHead>Tenant</TableHead>
                    <TableHead>Created</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {versions.data.map((v) => (
                    <TableRow key={`${v.tenant_id}:${v.version}`}>
                      <TableCell>v{v.version}</TableCell>
                      <TableCell>{v.display_name || "—"}</TableCell>
                      <TableCell className="max-w-xs truncate font-mono text-xs text-muted-foreground">
                        {v.source_type}: {v.source_ref}
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline">{v.tenant_id}</Badge>
                      </TableCell>
                      <TableCell className="text-muted-foreground">{formatTimestamp(v.created_at)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      )}

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete registry entry?</DialogTitle>
            <DialogDescription>
              Removes <span className="font-mono">{routeName}</span> from tenant{" "}
              <span className="font-mono">{form.tenant_id}</span>. Version history is deleted with it. This does not
              unregister any already-bootstrapped agents.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDelete} disabled={deleting}>
              {deleting && <Loader2 className="size-3.5 animate-spin" />}
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
