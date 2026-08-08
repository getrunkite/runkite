package auth

import (
	"net/http"
	"strings"
)

const agentRunPermPrefix = "agents:"
const agentRunPermSuffix = ":run"

// isAgentRunPermission reports whether p is agents:<agent_id>:run.
func isAgentRunPermission(p string) bool {
	if !strings.HasPrefix(p, agentRunPermPrefix) || !strings.HasSuffix(p, agentRunPermSuffix) {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(p, agentRunPermPrefix), agentRunPermSuffix)
	return mid != "" && !strings.Contains(mid, ":")
}

// AgentRunPermission builds the agents:<id>:run grant string.
func AgentRunPermission(agentID string) string {
	return agentRunPermPrefix + strings.TrimSpace(agentID) + agentRunPermSuffix
}

// isRunCreatePath reports POST routes that create a new run. An
// agents:<id>:run grant satisfies route-level "write" ONLY for these
// paths — never for DELETE/cancel/store/thread mutations. CanRunAgent
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
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
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
