package transport

// PoisonPill is a run that hit the reclaim generation ceiling and must
// NOT be re-enqueued. Its current assignment stays in flight until the
// control plane durably completes/finalizes the run and acknowledges it.
// Backend failures leave the assignment available to the next reclaim tick.
type PoisonPill struct {
	RunID string
	// Generation is the next blocked dispatch, not a new active attempt.
	// The retained assignment is Generation-1 and Ack must use that value.
	Generation int64
	AgentID    string
	TenantID   string
}
