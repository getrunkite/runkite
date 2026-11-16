package auth

import (
	"net/http"
	"strings"
)

const agentRunPermPrefix = "agents:"
const agentRunPermSuffix = ":run"
const agentObservePermSuffix = ":observe"

// isAgentRunPermission reports whether p is agents:<agent_id>:run.
func isAgentRunPermission(p string) bool { return isAgentPermission(p, agentRunPermSuffix) }

func isAgentObservePermission(p string) bool { return isAgentPermission(p, agentObservePermSuffix) }

func isAgentPermission(p, suffix string) bool {
	if !strings.HasPrefix(p, agentRunPermPrefix) || !strings.HasSuffix(p, suffix) {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(p, agentRunPermPrefix), suffix)
	return mid != "" && !strings.Contains(mid, ":")
}

// AgentRunPermission builds the agents:<id>:run grant string.
func AgentRunPermission(agentID string) string {
	return agentRunPermPrefix + agentID + agentRunPermSuffix
}

// isRunCreatePath reports POST routes that create a new run. An
// agents:<id>:run grant satisfies route-level "write" ONLY for these
// paths, never for DELETE/cancel/store/thread mutations. CanRunAgent
// then restricts which agent_id may be used inside createRunCtx.
//
// Matched:
//
//	POST /runs, /runs/stream, /runs/wait
//	POST /threads/{thread_id}/runs, .../runs/stream, .../runs/wait
//
// Not matched: /runs/search, cancel, delete, thread CRUD, store, etc.
func isRunCreatePath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	path = strings.TrimSuffix(path, "/")
	switch path {
	case "/runs", "/runs/stream", "/runs/wait":
		return true
	}
	const prefix = "/threads/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false
	}
	switch rest[slash+1:] {
	case "runs", "runs/stream", "runs/wait":
		return true
	default:
		return false
	}
}

// CanRunAgent reports whether the caller may create a run for agentID.
//
// Rules (mirrors authorized()'s empty-list semantics):
//   - nil result or empty permissions → unrestricted (true)
//   - "admin" or "write" → any agent
//   - "agents:<agentID>:run" → that agent only
//   - otherwise → false
func CanRunAgent(result *AuthResult, agentID string) bool {
	// Store IDs are exact strings. Normalizing them here could authorize a
	// different configured agent whose name has surrounding whitespace.
	if strings.TrimSpace(agentID) == "" {
		return false
	}
	if result == nil || len(result.Permissions) == 0 {
		return true
	}
	want := AgentRunPermission(agentID)
	for _, p := range result.Permissions {
		if p == "admin" || p == "write" || p == want {
			return true
		}
	}
	return false
}

// AgentObservePermission grants agent-wide retained run observation, separately
// from invocation. It does not grant thread history or requester-only privacy.
func AgentObservePermission(agentID string) string {
	return agentRunPermPrefix + agentID + agentObservePermSuffix
}

// CanReadAllRuns preserves ordinary read/write/admin and local-mode semantics.
// The HTTP middleware separately enforces strict empty-permission rejection.
func CanReadAllRuns(result *AuthResult) bool {
	if result == nil || len(result.Permissions) == 0 {
		return true
	}
	for _, p := range result.Permissions {
		if p == "read" || p == "write" || p == "admin" {
			return true
		}
	}
	return false
}

func CanObserveAgent(result *AuthResult, agentID string) bool {
	if CanReadAllRuns(result) {
		return true
	}
	if agentID == "" {
		return false
	}
	for _, p := range result.Permissions {
		if p == AgentObservePermission(agentID) {
			return true
		}
	}
	return false
}

// isRunObservationPath is deliberately limited to public exact GET and wait.
func isRunObservationPath(method, path string) bool {
	if method != http.MethodGet || !strings.HasPrefix(path, "/runs/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/runs/")
	if rest == "" {
		return false
	}
	parts := strings.Split(rest, "/")
	return (len(parts) == 1 && parts[0] != "" && parts[0] != "." && parts[0] != "..") ||
		(len(parts) == 2 && parts[0] != "" && parts[0] != "." && parts[0] != ".." && parts[1] == "wait")
}

func isMCPLifecyclePath(method, path string) bool {
	return path == "/mcp" && (method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete)
}

// Scoped run IDs can contain encoded slashes. Authorize the URL's segment shape,
// then let ServeMux decode the exact ID for the stored-agent handler check.
func scopedObservationRoute(result *AuthResult, method, escapedPath string) bool {
	if result == nil || !isRunObservationPath(method, escapedPath) {
		return false
	}
	for _, p := range result.Permissions {
		if isAgentObservePermission(p) {
			return true
		}
	}
	return false
}
