package main

// configuredAgentCapabilities describes the source tree's known framework
// profiles, not a connected worker or its effective configuration. The reserved
// fields are stable so repeating bootstrap does not manufacture agent versions.
func configuredAgentCapabilities(previous map[string]interface{}, frameworkKind, executionPool string) map[string]interface{} {
	capabilities := make(map[string]interface{}, len(previous)+1)
	for key, value := range previous {
		capabilities[key] = value
	}
	descriptor := map[string]interface{}{}
	if old, ok := previous["runkite_profile"].(map[string]interface{}); ok {
		for key, value := range old {
			descriptor[key] = value
		}
	}
	descriptor["schema_version"] = 1
	descriptor["provenance"] = "control_plane_configuration"
	descriptor["framework_kind"] = frameworkKind
	descriptor["execution_boundary"] = "direct_profile"
	descriptor["native_resume"] = "unknown"
	descriptor["checkpoint_ref"] = "unknown"
	descriptor["continuity"] = "unknown"
	descriptor["local_tool_allowlist"] = "unknown"
	descriptor["schema_reporting"] = "unknown"

	// A pool changes the execution boundary. Do not derive support from either
	// its dispatch name or an unqualified framework/backend combination.
	if executionPool != "" {
		descriptor["execution_boundary"] = "execution_pool"
	} else {
		switch frameworkKind {
		case "python-langgraph", "typescript-langgraphjs":
			descriptor["native_resume"] = "langgraph_checkpoint_required"
			descriptor["checkpoint_ref"] = "langgraph_checkpoint_required"
			descriptor["continuity"] = "langgraph_checkpoint_state"
			descriptor["local_tool_allowlist"] = "langgraph_stream_names"
			descriptor["schema_reporting"] = "bootstrap_stub"
			if frameworkKind == "python-langgraph" {
				descriptor["schema_reporting"] = "static_graph_introspection"
			}
		case "typescript-ai-sdk", "python-agent-framework", "python-openai-agents", "python-pydantic-ai", "python-google-adk":
			descriptor["native_resume"] = "unsupported"
			descriptor["checkpoint_ref"] = "unsupported"
			descriptor["continuity"] = "successful_turn_state_http"
			descriptor["local_tool_allowlist"] = "adapter_local_functions"
			descriptor["schema_reporting"] = "bootstrap_stub"
		}
	}
	capabilities["runkite_profile"] = descriptor
	return capabilities
}
