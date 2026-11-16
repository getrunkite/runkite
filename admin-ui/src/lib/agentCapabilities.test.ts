import { test } from "node:test";
import assert from "node:assert/strict";
import { agentProfileView, selectedAgentProfile } from "./agentCapabilities.ts";

const descriptor = {
  schema_version: 1,
  provenance: "control_plane_configuration",
  framework_kind: "python-langgraph",
  execution_boundary: "direct_profile",
  native_resume: "langgraph_checkpoint_required",
  checkpoint_ref: "langgraph_checkpoint_required",
  continuity: "langgraph_checkpoint_state",
  local_tool_allowlist: "langgraph_stream_names",
  schema_reporting: "static_graph_introspection",
};

test("configured LangGraph features retain checkpoint and tool-policy conditions", () => {
  const view = agentProfileView({ runkite_profile: descriptor });
  assert.equal(view.framework, "python-langgraph");
  assert.match(view.provenance, /configuration is unknown/);
  assert.equal(view.rows[0].value, "Checkpoint dependent");
  assert.match(view.rows[1].detail, /not verified/);
  assert.match(view.rows[2].detail, /in-memory/);
  assert.match(view.rows[3].detail, /not arbitrary in-graph code/);
  assert.match(view.rows[4].detail, /does not confirm a report arrived/);
});

test("managed native resume remains separate from connector approvals and committed history", () => {
  const view = agentProfileView({ runkite_profile: {
    ...descriptor, framework_kind: "python-openai-agents", native_resume: "unsupported",
    checkpoint_ref: "unsupported", continuity: "successful_turn_state_http",
    local_tool_allowlist: "adapter_local_functions", schema_reporting: "bootstrap_stub",
  } });
  assert.match(view.rows[0].value, /Unsupported/);
  assert.match(view.rows[0].detail, /connector approval is a separate path/);
  assert.match(view.rows[2].detail, /Unfinished turns are not restored/);
  assert.match(view.rows[3].detail, /not arbitrary application code/);
});

test("missing, malformed and future descriptors make no support claim", () => {
  for (const capabilities of [null, [], {}, { runkite_profile: "enabled" }, { runkite_profile: { ...descriptor, schema_version: 2 } }, { runkite_profile: { ...descriptor, provenance: "runner_live" } }]) {
    const view = agentProfileView(capabilities);
    assert.equal(view.framework, "Unknown");
    assert.ok(view.rows.every((row) => row.value === "Unknown"));
  }
});

test("unknown profiles, pool names and pooled known frameworks never infer support", () => {
  for (const profile of [
    { ...descriptor, framework_kind: "custom-agent" },
    { ...descriptor, framework_kind: "pool-python-langgraph" },
    { ...descriptor, execution_boundary: "execution_pool" },
    { ...descriptor, execution_boundary: "future_backend" },
  ]) {
    assert.ok(agentProfileView({ runkite_profile: profile }).rows.every((row) => row.value === "Unknown"));
  }
});

test("unknown feature codes stay unknown and metadata cannot provide guide URLs", () => {
  const view = agentProfileView({ runkite_profile: {
    ...descriptor, native_resume: "__proto__", checkpoint_ref: "https://other.invalid/approve",
    docs_url: "javascript:alert(1)", vendor: { nested: true },
  } });
  assert.equal(view.rows[0].value, "Unknown");
  assert.equal(view.rows[1].value, "Unknown");
  assert.deepEqual(view.rows.map((row) => row.guide), ["checkpoints.html", "checkpoints.html", "frameworks.html", "policy.html", "agents.html"]);
});

test("Try Agent selects exact tenant and agent identity without borrowing capabilities", () => {
  const agents = [
    { tenant_id: "first", agent_id: "same-name", capabilities: { runkite_profile: descriptor } },
    { tenant_id: "second", agent_id: "same-name", capabilities: {} },
  ];
  assert.equal(selectedAgentProfile(agents, "first", "same-name").framework, "python-langgraph");
  assert.equal(selectedAgentProfile(agents, "second", "same-name").framework, "Unknown");
  assert.equal(selectedAgentProfile(agents, "missing", "same-name").framework, "Unknown");
  assert.equal(selectedAgentProfile(agents, "first", "missing").framework, "Unknown");
});
