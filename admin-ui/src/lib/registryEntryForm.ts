import type { AdminRegistryEntry } from "../api/types.ts";

export interface EntryForm {
  tenant_id: string;
  name: string;
  display_name: string;
  description: string;
  author: string;
  tags: string;
  source_type: string;
  source_ref: string;
  metadata?: AdminRegistryEntry["metadata"];
}

export const emptyForm = (): EntryForm => ({
  tenant_id: "default",
  name: "",
  display_name: "",
  description: "",
  author: "",
  tags: "",
  source_type: "git",
  source_ref: "",
});

export function formFromEntry(e: AdminRegistryEntry): EntryForm {
  return {
    tenant_id: e.tenant_id,
    name: e.name,
    display_name: e.display_name ?? "",
    description: e.description ?? "",
    author: e.author ?? "",
    tags: (e.tags ?? []).join(", "),
    source_type: e.source_type,
    source_ref: e.source_ref,
    ...(e.metadata !== undefined ? { metadata: e.metadata } : {}),
  };
}

export function bodyFromForm(f: EntryForm): Omit<AdminRegistryEntry, "tenant_id" | "version" | "created_at" | "updated_at"> {
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
    // The form does not edit metadata, but PUT replaces the entry.
    ...(f.metadata !== undefined ? { metadata: f.metadata } : {}),
  };
}
