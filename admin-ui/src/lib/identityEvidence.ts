import type { AdminConnectorAction } from "../api/types.ts";

export interface IdentityEvidenceRow { label: string; value: string }

/** Fixed allowlist only. Never stringify arbitrary user/credential objects. */
export function identityEvidenceRows(action: Pick<AdminConnectorAction, "identity_provenance" | "workload_identity" | "delegation_contract">): IdentityEvidenceRow[] {
  const rows: IdentityEvidenceRow[] = [];
  const principal = action.identity_provenance;
  if (principal) {
    rows.push({ label: "Identity issuer at admission", value: principal.issuer }, { label: "Identity subject at admission", value: principal.subject }, { label: "Accepted audience", value: principal.audience }, { label: "Admission credential expiry", value: principal.expires_at });
    const actors = principal.issuer_asserted_actors?.slice(0, 4);
    if (actors?.length) rows.push({ label: "Issuer-asserted actors", value: actors.map((actor) => actor.issuer ? `${actor.subject} (${actor.issuer})` : actor.subject).join(" → ") });
  }
  const worker = action.workload_identity;
  if (worker) rows.push({ label: "Dispatch workload URI", value: worker.uri }, { label: "Dispatch public-key SHA256", value: worker.key_sha256 }, { label: "Dispatch certificate expiry", value: worker.not_after });
  const delegation = action.delegation_contract;
  if (delegation) {
    if (delegation.audiences?.length) rows.push({ label: "Registered exchange audiences", value: delegation.audiences.join(", ") });
    if (delegation.resources?.length) rows.push({ label: "Registered exchange resources", value: delegation.resources.join(", ") });
    rows.push({ label: "Registered exchange scopes", value: delegation.scopes.join(", ") }, { label: "Maximum exchange TTL", value: `${delegation.max_ttl_seconds} seconds` });
    if (delegation.configured_actor) rows.push({ label: "Operator-configured actor", value: delegation.configured_actor });
  }
  return rows;
}
