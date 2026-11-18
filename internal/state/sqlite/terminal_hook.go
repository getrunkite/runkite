package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/state"
)

var _ state.RunTerminalHookStore = (*SQLiteStore)(nil)

func (s *SQLiteStore) TryClaimRunTerminalHook(ctx context.Context, runID, incarnation string) (bool, error) {
	return s.completionDriver().TryClaimRunTerminalHook(ctx, runID, incarnation)
}
