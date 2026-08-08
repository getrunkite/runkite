// Mirrors internal/api/admin.go's response shapes and internal/models.
// Kept hand-written rather than codegen'd from Go structs -- the JSON
// surface here is small and stable enough that a codegen step would add
// more build complexity than it saves.

export interface AdminOverview {
  total_agents: number;
  total_threads: number;
  threads_by_status: Record<string, number>;
  total_runs: number;
  runs_by_status: Record<string, number>;
  connector_count: number;
  cron_schedule_count: number;
}

export interface AdminAgent {
  tenant_id: string;
  agent_id: string;
  name: string;
  description?: string;
  metadata?: Record<string, unknown>;
  capabilities?: Record<string, unknown>;
  version: number;
}

export interface AdminRegistryEntry {
  tenant_id: string;
  name: string;
  display_name?: string;
  description?: string;
  author?: string;
  tags?: string[];
  source_type: string;
  source_ref: string;
  metadata?: Record<string, unknown>;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface AdminRegistryEntryVersion {
  name: string;
  version: number;
  display_name?: string;
  description?: string;
  author?: string;
  tags?: string[];
  source_type: string;
  source_ref: string;
  created_at: string;
}

export interface AdminThread {
  tenant_id: string;
  thread_id: string;
  status: string;
  metadata: Record<string, unknown>;
  values?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface AdminRun {
  tenant_id: string;
  run_id: string;
  thread_id?: string;
  agent_id?: string;
  assistant_id?: string;
  status: string;
  created_at: string;
  updated_at: string;
  metadata: Record<string, unknown>;
  input?: unknown;
  config?: unknown;
  output?: unknown;
  error?: string;
}

export interface AdminConnector {
  name: string;
  type: string;
  mcp?: string;
  circuit_breaker_state?: string;
}

export interface AdminCronSchedule {
  tenant_id?: string;
  name: string;
  agent_id: string;
  expression: string;
  timezone: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface AdminWebhookDeadLetter {
  id: string;
  tenant_id: string;
  url: string;
  event_type: string;
  run_id: string;
  error: string;
  attempts: number;
  failed_at: string;
}

/** Policy decision row from GET /admin-api/audit-events (SQL backends). */
export interface AdminAuditEvent {
  id: string;
  ts: string;
  tenant_id: string;
  actor?: string;
  action: string;
  resource_type?: string;
  resource_id?: string;
  decision: string;
  reason_code?: string;
  rule_id?: string;
  latency_ms?: number;
  run_id?: string;
  generation?: number;
  agent_id?: string;
  connector?: string;
  tool?: string;
  attrs?: Record<string, unknown>;
  trace_id?: string;
}

/** Durable connector grant overlay from GET /admin-api/policy-grants. */
export interface AdminPolicyGrant {
  id: string;
  tenant_id: string;
  agent_id: string;
  connector: string;
  tools?: { allow?: string[]; deny?: string[] };
  created_at?: string;
  updated_at?: string;
}

/** Connector HITL row from GET /admin-api/pending-actions. */
export interface AdminPendingAction {
  id: string;
  run_id: string;
  generation: number;
  tenant_id: string;
  agent_id: string;
  connector: string;
  tool: string;
  rule_id?: string;
  reason?: string;
  reason_code?: string;
  status: string;
  created_at: string;
  updated_at: string;
}

/** Active kill/pause flag from GET /admin-api/kill-switches. */
export interface AdminKillSwitch {
  id: string;
  tenant_id: string;
  agent_id?: string;
  pause_only: boolean;
  reason?: string;
  created_by?: string;
  created_at?: string;
  updated_at?: string;
}

/** POST /admin-api/kill-switches response. */
export interface AdminKillSwitchCreateResponse {
  kill_switch: AdminKillSwitch;
  cancelled: number;
}

/** Active break-glass window from GET /admin-api/break-glass. */
export interface AdminBreakGlassWindow {
  id: string;
  tenant_id: string;
  agent_id?: string;
  reason: string;
  created_by?: string;
  starts_at: string;
  expires_at: string;
  created_at?: string;
  updated_at?: string;
}
