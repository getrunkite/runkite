package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/state/migrate"
)

func (s *Store) upPendingTargetRevision(ctx context.Context, db migrate.DB) error {
	return s.addColumnIfMissing(ctx, db, "pending_actions", "target_revision", `VARCHAR(74) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''`)
}
