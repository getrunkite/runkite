package transport

// PoisonPill is a run that hit the reclaim generation ceiling and must
// NOT be re-enqueued. The control-plane reclaim loop calls StatusCallback
// with status=error for each of these so Postgres, thread release, hooks,
// and SSE closure stay consistent with a runner-reported failure.
type PoisonPill struct {
	RunID      string
	Generation int64
	AgentID    string
	TenantID   string
}
