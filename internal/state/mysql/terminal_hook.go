package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/state"
)

var _ state.RunTerminalHookStore = (*Store)(nil)

func (s *Store) TryClaimRunTerminalHook(ctx context.Context, runID, incarnation string) (bool, error) {
	return s.completionDriver().TryClaimRunTerminalHook(ctx, runID, incarnation)
}
