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
  // undefined is a new form; null means an older replica omitted the raw field.
  metadata_json?: string | null;
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
    metadata_json: e.metadata_json ?? null,
  };
}

export function jsonBodyFromForm(f: EntryForm): string {
  const tags = f.tags
    .split(/[,\s]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const body = {
    name: f.name.trim(),
    display_name: f.display_name.trim() || undefined,
    description: f.description.trim() || undefined,
    author: f.author.trim() || undefined,
    tags: tags.length ? tags : undefined,
    source_type: f.source_type.trim(),
    source_ref: f.source_ref.trim(),
  };
  const encoded = JSON.stringify(body);
  if (f.metadata_json === undefined) return encoded;
  if (f.metadata_json === null) {
    throw new Error("Reload this entry from an updated server before saving its metadata.");
  }
  let metadata: unknown;
  try {
    metadata = JSON.parse(f.metadata_json);
  } catch {
    throw new Error("Reload this entry before saving: stored metadata JSON is unavailable.");
  }
  if (metadata !== null && (typeof metadata !== "object" || Array.isArray(metadata))) {
    throw new Error("Reload this entry before saving: stored metadata must be an object or null.");
  }
  // Validation above never becomes serialization. The parsed object may contain
  // rounded numbers; splice only the original fragment at this fixed property.
  return `${encoded.slice(0, -1)},"metadata":${f.metadata_json}}`;
}
