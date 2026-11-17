package state

import "context"

// RunTerminalHookStore claims terminal hook delivery for an exact stored run.
// It is optional so custom stores do not silently inherit an unfenced fallback.
type RunTerminalHookStore interface {
	// TryClaimRunTerminalHook verifies the tenant-scoped current run is terminal
	// and has the supplied nonempty original identity in the same transaction
	// that claims delivery. A matching previous claim returns false, nil.
	// Missing, replaced, nonterminal or legacy-ambiguous records fail closed.
	// A legacy bare run-ID claim cannot establish which record was finalized;
	// callers must reconcile it and use fresh operation IDs, never backfill it.
	TryClaimRunTerminalHook(ctx context.Context, runID, incarnation string) (bool, error)
}
