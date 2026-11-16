import type { AdminPolicyGrant } from "../api/types.ts";

export interface GrantForm {
  id: string;
  tenant_id: string;
  agent_id: string;
  connector: string;
  allow: string;
  deny: string;
}

export const emptyForm = (): GrantForm => ({
  id: "",
  tenant_id: "",
  agent_id: "",
  connector: "",
  allow: "",
  deny: "",
});

export function formFromGrant(g: AdminPolicyGrant): GrantForm {
  return {
    id: g.id,
    tenant_id: g.tenant_id,
    agent_id: g.agent_id,
    connector: g.connector,
    allow: g.tools?.allow?.length ? JSON.stringify(g.tools.allow) : "",
    deny: g.tools?.deny?.length ? JSON.stringify(g.tools.deny) : "",
  };
}

function toolNames(raw: string): string[] {
  if (!raw.trim()) return [];
  let value: unknown;
  try { value = JSON.parse(raw); }
  catch { throw new Error("Tool names must be a JSON array of nonempty strings."); }
  if (!Array.isArray(value) || value.some((name) => typeof name !== "string" || name.length === 0))
    throw new Error("Tool names must be a JSON array of nonempty strings.");
  return value;
}

export function bodyFromForm(f: GrantForm): AdminPolicyGrant {
  const allow = toolNames(f.allow);
  const deny = toolNames(f.deny);
  const out: AdminPolicyGrant = {
    id: f.id,
    tenant_id: f.tenant_id,
    agent_id: f.agent_id,
    connector: f.connector,
  };
  if (allow.length || deny.length) {
    out.tools = {};
    if (allow.length) out.tools.allow = allow;
    if (deny.length) out.tools.deny = deny;
  }
  return out;
}
