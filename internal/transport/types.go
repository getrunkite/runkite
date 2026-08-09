// Package transport defines the interfaces for job dispatch and event delivery
// between the control plane and runners. Every transport implementation (in-memory,
// queue/Redis, gRPC long-poll) must satisfy these interfaces and pass the shared
// conformance test suite in transport/conformance.
package transport

import (
	"context"
	"encoding/json"
	"time"
)

// RunAssignment is a job dispatched from the control plane to a runner.
// Fields match runner-protocol/schemas/run_assignment.json.
type RunAssignment struct {
	RunID          string          `json:"run_id"`
	ThreadID       string          `json:"thread_id"`
	RunnerKind     string          `json:"runner_kind"`
	GraphID        string          `json:"graph_id"`
	Input          json.RawMessage `json:"input"`
	Config         json.RawMessage `json:"config,omitempty"`
	CheckpointRef  *string         `json:"checkpoint_ref"`
	ResumeCommand  json.RawMessage `json:"resume_command"`
	StreamModes    []string        `json:"stream_modes"`
	ConnectorNeeds []string        `json:"connector_needs"`
	TraceContext   *TraceContext   `json:"trace_context,omitempty"`
	// User carries the identity that authenticated this run's HTTP
	// request, if any. Framework-specific runners may map this onto
	// their own runtime identity object (e.g. LangGraph's
	// runtime.user for per-request factory graphs). Nil when no auth
	// provider is configured, or the caller has no identity attached.
	User *UserContext `json:"user,omitempty"`

	// TenantID scopes direct-mode store/vector SQL to the same tenant
	// the control plane used when creating the run. On run-bound
	// /internal/* proxy paths the control plane re-reads this from the
	// in-flight assignment (runners still echo X-Runkite-Tenant-Id for
	// unbound routes and older control planes). Empty/absent means
	// "default". LangGraph's own checkpoint tables remain unscoped --
	// see docs/auth.md Multi-tenancy.
	TenantID string `json:"tenant_id,omitempty"`

	// Generation fences a job against a runner that gets reclaimed
	// while genuinely still executing, then finishes anyway and
	// reports a stale result after a second runner already took over.
	// Without this, nothing stops an old, superseded runner's late
	// report from clobbering or duplicating the runner that replaced
	// it. Starts at 1 when a run is first created
	// (internal/api/runs.go's createRunCtx); ReclaimStale increments it
	// every time it re-enqueues a stale job, atomically with the
	// re-enqueue itself (both transports) so the incremented value is
	// what the NEXT runner to Dequeue this job actually receives.
	//
	// The runner echoes whatever generation it was handed back on
	// every RPC that identifies this specific dispatch attempt
	// (Heartbeat, the first StreamEvents message, ReportStatus) --
	// see JobQueue.Ack/Renew's own doc comments for how the control
	// plane uses this to reject a stale attempt's report instead of
	// letting it overwrite (or duplicate work alongside) whatever the
	// current attempt is doing. A runner build that predates fencing
	// simply never sends this field, which unmarshals to the Go zero
	// value 0 -- treated as "always matches" for backward compat, not
	// as a real generation number (see the same reasoning in
	// Ack/Renew).
	Generation int64 `json:"generation,omitempty"`
}

// UserContext is the identity that authenticated a run's originating
// request, forwarded from auth.AuthResult (see internal/auth/auth.go) so
// a runner can reconstruct a runtime.user-shaped object without the
// control plane needing to know anything about any specific runner's
// auth/runtime types.
//
// On the wire, Extra is flattened into the same JSON object as identity
// (not nested under "extra"). That matches LangGraph Platform's own flat
// user payload convention so runner-side code can do
// user.to_dict().get("email") or .get("sso_token") without knowing about
// an internal nesting convention. Go keeps Extra as a separate field for
// typed access.
type UserContext struct {
	Identity        string
	DisplayName     string
	IsAuthenticated bool
	Permissions     []string
	Extra           map[string]interface{}
}

// MarshalJSON flattens Extra into the top-level user object.
func (u UserContext) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{
		"identity":         u.Identity,
		"is_authenticated": u.IsAuthenticated,
	}
	if u.DisplayName != "" {
		m["display_name"] = u.DisplayName
	}
	if len(u.Permissions) > 0 {
		m["permissions"] = u.Permissions
	}
	for k, v := range u.Extra {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON accepts both the flat wire format and a legacy nested
// {"extra": {...}} shape (defense for in-flight queue messages).
func (u *UserContext) UnmarshalJSON(data []byte) error {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if v, ok := m["identity"].(string); ok {
		u.Identity = v
	}
	if v, ok := m["display_name"].(string); ok {
		u.DisplayName = v
	}
	if v, ok := m["is_authenticated"].(bool); ok {
		u.IsAuthenticated = v
	}
	if raw, ok := m["permissions"]; ok {
		switch arr := raw.(type) {
		case []interface{}:
			for _, item := range arr {
				if s, ok := item.(string); ok {
					u.Permissions = append(u.Permissions, s)
				}
			}
		case []string:
			u.Permissions = append(u.Permissions, arr...)
		}
	}
	reserved := map[string]bool{
		"identity": true, "display_name": true,
		"is_authenticated": true, "permissions": true, "extra": true,
	}
	extra := make(map[string]interface{})
	if nested, ok := m["extra"].(map[string]interface{}); ok {
		for k, v := range nested {
			extra[k] = v
		}
	}
	for k, v := range m {
		if reserved[k] {
			continue
		}
		extra[k] = v
	}
	if len(extra) > 0 {
		u.Extra = extra
	}
	return nil
}

// TraceContext carries W3C Trace Context for cross-process observability.
type TraceContext struct {
	Traceparent   string `json:"traceparent"`
	Tracestate    string `json:"tracestate"`
	CorrelationID string `json:"correlation_id"`
}

// RunEvent is an event published by a runner back to the control plane.
// Fields match runner-protocol/schemas/run_event.json.
type RunEvent struct {
	EventID   string          `json:"event_id"`
	Seq       int64           `json:"seq"`
	Method    string          `json:"method"`
	Namespace []string        `json:"namespace"`
	Data      json.RawMessage `json:"data"`
	Ts        int64           `json:"ts"`
}

// IsTerminal returns true if this event ends the run (end or error method).
func (e *RunEvent) IsTerminal() bool {
	return e.Method == "end" || e.Method == "error"
}

// JobQueue handles dispatching RunAssignments to runners.
// Implementations: in-memory (conformance testing), Redis (production), gRPC long-poll.
type JobQueue interface {
	// Enqueue adds a job to the queue. Returns immediately.
	Enqueue(ctx context.Context, job *RunAssignment) error

	// Dequeue blocks until a job is available or timeout expires.
	// Returns nil, nil if timeout expires with no job.
	Dequeue(ctx context.Context, runnerKind string, timeout time.Duration) (*RunAssignment, error)

	// Ack acknowledges successful processing of a job, fenced by
	// generation (see RunAssignment.Generation's own doc comment for
	// the full rationale): only removes the job from in-flight
	// tracking if generation matches the CURRENT in-flight generation
	// for runID, OR generation is 0 (a pre-fencing runner build,
	// trusted unconditionally the same way this method always behaved
	// before fencing existed). accepted=false means the report must
	// NOT be applied to run state -- either a newer attempt already
	// exists (this one was reclaimed and superseded), or nothing is
	// tracked for runID at all (already finished by the real owner, or
	// never existed) -- both are "not this caller's job to finalize
	// anymore," not an error.
	Ack(ctx context.Context, runID string, generation int64) (accepted bool, err error)

	// Renew extends an in-flight job's lease (resets its staleness clock)
	// WITHOUT removing it from tracking, unlike Ack. Backs the runner
	// heartbeat mechanism: a runner calls this periodically for the
	// WHOLE duration of a job's execution, not just once at the start,
	// so a stale-job reaper's "time since last touch" check reflects
	// real liveness throughout execution instead of just the window
	// between Dequeue and the first StreamEvents message.
	//
	// Also fenced by generation, same rules as Ack: current=false means
	// this runner has been superseded (a newer generation is now the
	// one being tracked) or nothing is tracked for runID at all
	// (finished, reclaimed away with no successor yet, or canceled) --
	// callers (the Heartbeat RPC handler) should treat current=false
	// as "stop executing," not just "retry the heartbeat." generation
	// 0 always returns current=true if the job is in-flight at all
	// (pre-fencing runner build, same unconditional-trust behavior
	// this method always had before fencing existed). Still never an
	// error just because the job is no longer in-flight at all
	// (already Ack'd, reclaimed, or canceled) -- a late or racing
	// renewal for a job that already finished must not resurrect it,
	// it just gets current=false instead of true.
	Renew(ctx context.Context, runID string, generation int64) (current bool, err error)

	// Nack signals that the job was not processed successfully.
	// The job should be made available for re-delivery.
	Nack(ctx context.Context, runID string) error

	// Cancel removes or poisons a job so it is not delivered to any runner.
	// If already dequeued, the runner must do a pre-execution status check.
	Cancel(ctx context.Context, runID string) error

	// Len returns the number of jobs currently in the queue.
	Len(ctx context.Context) (int64, error)

	// Ping verifies the underlying broker (Redis, NATS, Kafka) is
	// actually reachable right now -- a real round trip, not just "the
	// client was constructed without error at startup." Backs GET
	// /readyz: unlike Len (which for Redis is SCAN-based and
	// deliberately kept off any hot path -- see cmd/serve.go's
	// pollQueueDepth), Ping is meant to be cheap enough to call on
	// every readiness probe.
	Ping(ctx context.Context) error

	// LookupInflight returns the currently in-flight RunAssignment for
	// runID, or (nil, nil) if nothing is tracked (never dequeued, already
	// Ack'd, canceled, or unknown). Used by /internal/* run-binding so
	// connector/store/vector calls can prove they belong to an active
	// dispatch and derive tenant/agent from the assignment instead of
	// trusting runner-supplied headers.
	LookupInflight(ctx context.Context, runID string) (*RunAssignment, error)
}

// EventBroker handles publishing and subscribing to run events.
// Supports fan-out (multiple subscribers per run) and replay.
type EventBroker interface {
	// Publish sends an event for a run. All subscribers to this runID receive it.
	Publish(ctx context.Context, runID string, event *RunEvent) error

	// Subscribe returns a channel that receives events for the given runID.
	// The channel is closed when the run ends or Close is called.
	Subscribe(ctx context.Context, runID string) (<-chan *RunEvent, error)

	// Replay returns stored events for a run starting after the given sequence number.
	// If sinceSeq is 0, returns all stored events.
	Replay(ctx context.Context, runID string, sinceSeq int64) ([]*RunEvent, error)

	// Close signals that a run is finished. Closes all subscriber channels
	// and marks the run's event stream as complete.
	Close(runID string) error
}

// CancelBroker handles cancel signal delivery to runners.
type CancelBroker interface {
	// PublishCancel sends a cancel signal for the given runID.
	PublishCancel(ctx context.Context, runID string) error

	// SubscribeCancel returns a channel that fires when a cancel is
	// received for the given runID. The channel delivers at most one
	// value, and is never closed by a ctx cancellation alone (only a
	// genuine cancel signal both sends a value AND closes it) -- a
	// caller selecting on both this channel and ctx.Done() must be able
	// to tell "real cancel" apart from "I stopped waiting" unambiguously.
	//
	// ctx governs this subscription's OWN lifetime, not just the call:
	// implementations MUST release any resources held for this
	// subscription (goroutines, connections, map entries) when ctx is
	// done, not only when a cancel arrives. Callers MUST pass a context
	// that is cancelled once they stop caring about this run (e.g. on
	// run completion) -- context.Background() here leaks the
	// subscription for the run's entire un-cancelled lifetime, which
	// for a run that completes normally (the common case) means
	// forever. Confirmed via pprof under load: this exact mistake in
	// internal/bridge/server.go's GetJob leaked one Redis PubSub
	// subscription + 2 goroutines per run permanently, the real root
	// cause of the Redis transport's concurrency-dependent latency
	// blowup documented in bench/REPORT.md.
	SubscribeCancel(ctx context.Context, runID string) (<-chan struct{}, error)
}
