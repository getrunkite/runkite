package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// Headers runners must send on run-bound /internal/* proxy paths so the
// control plane can prove the call belongs to an active dispatch.
const (
	HeaderRunID      = "X-Runkite-Run-Id"
	HeaderGeneration = "X-Runkite-Generation"
)

// Stable reason_code values for run-binding denials (and related
// /internal/* auth failures). Clients and SIEM can key off these
// without parsing free-form message text.
const (
	ReasonRunnerCredentialsInvalid = "runner_credentials_invalid"
	ReasonRunnerTenantDenied       = "runner_tenant_denied"
	ReasonRunBindingRequired       = "run_binding_required"
	ReasonRunNotInflight           = "run_not_inflight"
	ReasonRunGenerationMismatch    = "run_generation_mismatch"
	ReasonRunBindingLookupFailed   = "run_binding_lookup_failed"
	ReasonRunThreadMismatch        = "run_thread_mismatch"
)

// InflightLookup resolves the currently in-flight RunAssignment for a
// run id. Satisfied by transport.JobQueue; kept as a narrow interface
// so auth tests can stub without a full queue.
type InflightLookup interface {
	LookupInflight(ctx context.Context, runID string) (*transport.RunAssignment, error)
}

// RunBinding is the control-plane-derived identity for a run-bound
// /internal/* call. Tenant/agent/thread come from the assignment, not
// from runner-supplied headers.
type RunBinding struct {
	RunID      string
	Generation int64
	TenantID   string
	AgentID    string
	ThreadID   string
	User       *transport.UserContext
}

type runBindingKey struct{}

// WithRunBinding attaches a resolved RunBinding to ctx.
func WithRunBinding(ctx context.Context, b *RunBinding) context.Context {
	return context.WithValue(ctx, runBindingKey{}, b)
}

// RunBindingFromContext returns the RunBinding set by middleware, or nil.
func RunBindingFromContext(ctx context.Context) *RunBinding {
	b, _ := ctx.Value(runBindingKey{}).(*RunBinding)
	return b
}

// requiresRunBinding reports whether path is a proxy surface that must
// prove an active run (connector session/MCP, store, vectors). Other
// /internal/* routes (schema publish, run status, A2A, connector
// metadata) stay runner-token-only -- they are not always issued under
// an in-flight assignment.
func requiresRunBinding(path string) bool {
	if strings.HasPrefix(path, "/internal/store") ||
		strings.HasPrefix(path, "/internal/vectors") ||
		strings.HasPrefix(path, "/internal/checkpoints") {
		return true
	}
	if strings.HasPrefix(path, "/internal/connectors/") {
		return strings.HasSuffix(path, "/session") || strings.HasSuffix(path, "/mcp")
	}
	return false
}

// bindInternalRun validates run headers against Inflight and returns a
// context with tenant (+ optional principal) derived from the
// assignment. Call only when opts.Inflight != nil and requiresRunBinding.
func bindInternalRun(w http.ResponseWriter, r *http.Request, kind string, tokens *RunnerTokens, inflight InflightLookup) (context.Context, bool) {
	runID := strings.TrimSpace(r.Header.Get(HeaderRunID))
	genRaw := strings.TrimSpace(r.Header.Get(HeaderGeneration))
	if runID == "" || genRaw == "" {
		writeDenied(w, http.StatusUnauthorized, ReasonRunBindingRequired,
			"missing X-Runkite-Run-Id or X-Runkite-Generation")
		return nil, false
	}
	generation, err := strconv.ParseInt(genRaw, 10, 64)
	if err != nil {
		writeDenied(w, http.StatusUnauthorized, ReasonRunBindingRequired,
			"invalid X-Runkite-Generation")
		return nil, false
	}

	assignment, err := inflight.LookupInflight(r.Context(), runID)
	if err != nil {
		writeDenied(w, http.StatusServiceUnavailable, ReasonRunBindingLookupFailed,
			"failed to resolve run assignment")
		return nil, false
	}
	if assignment == nil {
		writeDenied(w, http.StatusForbidden, ReasonRunNotInflight,
			"run is not in-flight")
		return nil, false
	}
	// generation 0 from either side bypasses fencing -- same pre-fencing
	// runner compat as Ack/Renew.
	if generation != 0 && assignment.Generation != 0 && generation != assignment.Generation {
		writeDenied(w, http.StatusForbidden, ReasonRunGenerationMismatch,
			"run generation mismatch")
		return nil, false
	}

	tenantID := strings.TrimSpace(assignment.TenantID)
	if tenantID == "" {
		tenantID = tenant.DefaultTenant
	}
	if tokens.Enabled() && !tokens.AllowsTenant(kind, tenantID) {
		writeDenied(w, http.StatusForbidden, ReasonRunnerTenantDenied,
			"tenant not allowed for this runner kind")
		return nil, false
	}

	binding := &RunBinding{
		RunID:      runID,
		Generation: assignment.Generation,
		TenantID:   tenantID,
		AgentID:    assignment.GraphID,
		ThreadID:   assignment.ThreadID,
		User:       assignment.User,
	}
	ctx := tenant.WithContext(r.Context(), tenantID)
	ctx = WithRunBinding(ctx, binding)
	if assignment.User != nil && assignment.User.Identity != "" {
		ctx = WithContext(ctx, &AuthResult{
			Identity:    assignment.User.Identity,
			Permissions: assignment.User.Permissions,
			TenantID:    tenantID,
			DisplayName: assignment.User.DisplayName,
			Extra:       assignment.User.Extra,
		})
	}
	return ctx, true
}

func writeDenied(w http.ResponseWriter, status int, reasonCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message":     message,
		"reason_code": reasonCode,
	})
}
