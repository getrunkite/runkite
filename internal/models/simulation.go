package models

import "strings"

// RunIsSimulation reports whether a run was tagged as fixture replay.
// The control plane writes JSON boolean true. String "true"/"1"/"yes"
// is accepted so Admin badges and A2A inherit still work if a row was
// written by hand. Missing or any other value is not a simulation.
func RunIsSimulation(meta map[string]interface{}) bool {
	if meta == nil {
		return false
	}
	v, ok := meta["simulation"]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}
