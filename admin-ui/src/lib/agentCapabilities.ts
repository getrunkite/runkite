type ProfileRow = { key: string; label: string; value: string; detail: string; guide: string };
export type AgentProfileView = { framework: string; provenance: string; rows: ProfileRow[] };

const knownProfiles = new Set([
  "python-langgraph", "typescript-langgraphjs", "typescript-ai-sdk",
  "python-agent-framework", "python-openai-agents", "python-pydantic-ai", "python-google-adk",
]);

const fields = [
  ["native_resume", "Native interrupt resume", "checkpoints.html"],
  ["checkpoint_ref", "Checkpoint time travel", "checkpoints.html"],
  ["continuity", "Conversation continuity", "frameworks.html"],
  ["local_tool_allowlist", "Local tool allowlist", "policy.html"],
  ["schema_reporting", "Schema reporting", "agents.html"],
] as const;

const descriptions: Record<string, Record<string, readonly [string, string]>> = {
  native_resume: {
    langgraph_checkpoint_required: ["Checkpoint dependent", "LangGraph interrupt resume requires a usable checkpoint and compatible thread identity."],
    unsupported: ["Unsupported in this profile", "Native framework approval/resume is unsupported. Plane connector approval is a separate path."],
  },
  checkpoint_ref: {
    langgraph_checkpoint_required: ["Checkpoint dependent", "The runner can select a LangGraph checkpoint. Availability, saver mode and thread compatibility are not verified here."],
    unsupported: ["Unsupported in this profile", "This adapter refuses checkpoint_ref. Successful-turn history is not checkpoint time travel."],
  },
  continuity: {
    langgraph_checkpoint_state: ["LangGraph checkpoint state", "Durability depends on the runner's saver. Direct, HTTP and in-memory modes are not observed here."],
    successful_turn_state_http: ["Successful turns over HTTP", "The adapter requires HTTP checkpoint storage for committed history or session state. Unfinished turns are not restored."],
  },
  local_tool_allowlist: {
    langgraph_stream_names: ["Conditional: observed graph names", "Only when allowed_tools is configured. It checks observed graph tool calls, not arbitrary in-graph code. Connector policy is separate."],
    adapter_local_functions: ["Conditional: local-function names", "Only when allowed_tools is configured. It covers supported local function calls, not arbitrary application code. Connector policy is separate."],
  },
  schema_reporting: {
    static_graph_introspection: ["Static Python graph introspection", "Static graphs can report schemas after load; factories and failed or missing reports retain a stub. This does not confirm a report arrived."],
    bootstrap_stub: ["Bootstrap stub by default", "The shipped adapter does not introspect agent schemas. Check the schema response separately for any supplied report."],
  },
};

function object(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

/** Read advisory configuration only. Unknown formats and profiles make no feature claim. */
export function agentProfileView(capabilities: unknown): AgentProfileView {
  const descriptor = object(object(capabilities)?.runkite_profile);
  const valid = descriptor?.schema_version === 1 && descriptor.provenance === "control_plane_configuration";
  const framework = valid && typeof descriptor.framework_kind === "string" && descriptor.framework_kind.length <= 120
    ? descriptor.framework_kind : "Unknown";
  const direct = valid && descriptor.execution_boundary === "direct_profile";
  const known = direct && knownProfiles.has(framework);
  const provenance = !valid ? "No supported configured-profile descriptor."
    : descriptor.execution_boundary === "execution_pool" ? "Configured framework in an execution pool; feature support is unknown."
    : direct ? "Configured framework profile; effective runner configuration is unknown."
    : "Configured framework; execution boundary is unknown.";

  return {
    framework,
    provenance,
    rows: fields.map(([key, label, guide]) => {
      const code = known && typeof descriptor?.[key] === "string" ? descriptor[key] as string : "unknown";
      const [value, detail] = Object.hasOwn(descriptions[key], code) ? descriptions[key][code]
        : ["Unknown", "No feature claim is available for this configured profile or execution boundary."];
      return { key, label, value, detail, guide };
    }),
  };
}

/** Never borrow a same-name agent's descriptor from another tenant. */
export function selectedAgentProfile(
  agents: readonly { tenant_id: string; agent_id: string; capabilities?: unknown }[],
  tenantId: string,
  agentId: string,
): AgentProfileView {
  return agentProfileView(agents.find((agent) => agent.tenant_id === tenantId && agent.agent_id === agentId)?.capabilities);
}
