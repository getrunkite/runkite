/** Presentation and scope checks for metadata-only managed connector evidence. */
export const CONNECTOR_ACTION_STATES = [
  "received",
  "blocked",
  "approval_required",
  "ready",
  "not_dispatched",
  "dispatch_intent",
  "response_observed",
  "outcome_unknown",
] as const;

export type ActionTone = "success" | "warning" | "destructive" | "secondary" | "muted";

const STATES: Record<string, { label: string; tone: ActionTone; description: string }> = {
  received: {
    label: "Received",
    tone: "muted",
    description: "The plane recorded the request. No forwarding authority or dispatch is established by this state.",
  },
  blocked: { label: "Blocked", tone: "destructive", description: "The plane blocked this request before forwarding." },
  approval_required: {
    label: "Approval required",
    tone: "warning",
    description: "This request needs approval. Approval alone does not execute a tool.",
  },
  ready: {
    label: "Ready",
    tone: "secondary",
    description: "Authority was recorded. No dispatch intent has been recorded yet.",
  },
  not_dispatched: {
    label: "Not dispatched",
    tone: "muted",
    description: "The plane recorded that forwarding did not start.",
  },
  dispatch_intent: {
    label: "Dispatch intent",
    tone: "warning",
    description: "The plane recorded intent to forward. This does not prove that the upstream received the request.",
  },
  response_observed: {
    label: "Response observed",
    tone: "secondary",
    description: "The plane received a response. This does not prove the intended business effect succeeded.",
  },
  outcome_unknown: {
    label: "Outcome unknown",
    tone: "warning",
    description: "The tool may have executed. Check the downstream system before deciding whether to retry.",
  },
};

export function connectorActionState(state: string) {
  return (
    (Object.hasOwn(STATES, state) ? STATES[state] : undefined) ?? {
      label: "Unrecognized state",
      tone: "muted" as ActionTone,
      description:
        "This Admin version cannot interpret the recorded state. Inspect the API version before drawing a conclusion.",
    }
  );
}

export function connectorAuthority(source?: string): string {
  switch (source) {
    case "policy":
      return "Policy gate";
    case "approved_capability":
      return "Approved capability";
    case "break_glass":
      return "Break-glass";
    default:
      return source || "No authority recorded";
  }
}

export interface ConnectorActionFilters {
  tenant_id?: string;
  run_id?: string;
  connector?: string;
  state?: string;
}

export function connectorActionsPath(filters: ConnectorActionFilters = {}, cursor?: string, limit = 50): string {
  const query = actionFilterQuery(filters);
  query.set("limit", String(limit));
  if (cursor) query.set("cursor", cursor);
  return `/connector-actions?${query}`;
}

export function connectorActionsHref(filters: ConnectorActionFilters = {}): string {
  const query = actionFilterQuery(filters).toString();
  return `/admin/connector-actions${query ? `?${query}` : ""}`;
}

function actionFilterQuery(filters: ConnectorActionFilters): URLSearchParams {
  const query = new URLSearchParams();
  for (const name of ["tenant_id", "run_id", "connector", "state"] as const) {
    const value = filters[name];
    if (value !== undefined && value !== "") query.set(name, value);
  }
  return query;
}

export function connectorActionPath(actionId: string): string {
  return `/connector-actions/${encodeURIComponent(actionId)}`;
}

export function connectorApprovalQueueHref(tenantId: string, runId: string): string {
  return `/admin/pending?${new URLSearchParams({ tenant_id: tenantId, run_id: runId, status: "all" })}`;
}

export function connectorActionScope(filters: ConnectorActionFilters): string {
  return actionFilterQuery(filters).toString();
}

/** Only the non-reused database identity can prove a run incarnation. */
export function connectorActionMatchesRun(
  action: { tenant_id: string; run_id: string; run_incarnation_id: string },
  run: { tenant_id: string; run_id: string; incarnation_id?: string },
): boolean {
  return (
    action.tenant_id === run.tenant_id &&
    action.run_id === run.run_id &&
    typeof action.run_incarnation_id === "string" &&
    action.run_incarnation_id.length > 0 &&
    action.run_incarnation_id === run.incarnation_id
  );
}

/** A run link is safe only after the server verifies the original incarnation. */
export function connectorActionRunHref(
  action: { run_id: string; tenant_id: string; run_incarnation_id: string },
  runAvailable: boolean,
): string | null {
  if (runAvailable !== true || !action.run_incarnation_id) return null;
  const expected = new URLSearchParams({
    expected_tenant_id: action.tenant_id,
    expected_incarnation_id: action.run_incarnation_id,
  });
  return `/admin/runs/${encodeURIComponent(action.run_id)}?${expected}`;
}

/** Recheck the referenced incarnation when a retained-evidence run link opens. */
export function connectorActionRunReferenceMatches(
  run: { tenant_id: string; run_id: string; incarnation_id?: string },
  runId: string,
  expectedTenant: string | null,
  expectedIncarnation: string | null,
): boolean {
  if (expectedTenant === null && expectedIncarnation === null) return true;
  if (expectedTenant === null || expectedIncarnation === null) return false;
  return connectorActionMatchesRun(
    { tenant_id: expectedTenant, run_id: runId, run_incarnation_id: expectedIncarnation },
    run,
  );
}

/** Scope-tagged results prevent old rows flashing beneath a newly selected run. */
export function evidenceForPath<T>(value: { path: string; data: T | null }, path: string): T | null {
  return value.path === path ? value.data : null;
}
