package auth

import (
	"net/http"
	"net/url"
	"strings"
)

// A pool can report schemas for its own agents before it has an assignment.
// Every data-bearing pool route otherwise requires active run binding.
func poolSchemaAgentAllowed(tokens *RunnerTokens, kind string, r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/internal/agents/") || !strings.HasSuffix(path, "/schema") {
		return false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, "/internal/agents/"), "/schema")
	if raw == "" || strings.Contains(raw, "/") {
		return false
	}
	agent, err := url.PathUnescape(raw)
	return err == nil && tokens.AllowsPoolAgent(kind, agent)
}

func poolRunPathAllowed(r *http.Request, binding *RunBinding) bool {
	if binding == nil {
		return false
	}
	if requiresRunBinding(r.URL.Path) {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/internal/runs/") || !strings.HasSuffix(path, "/status") {
		return false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, "/internal/runs/"), "/status")
	if strings.Contains(raw, "/") {
		return false
	}
	runID, err := url.PathUnescape(raw)
	return err == nil && runID == binding.RunID
}
