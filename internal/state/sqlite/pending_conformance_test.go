package sqlite

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/state/conformance"
)

func TestPendingSecurityConformance(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	conformance.RunPendingSecurity(t, s)
	conformance.RunPendingTenantScope(t, s)
	conformance.RunPendingTargetRevisionSecurity(t, s, func(ctx context.Context, id, revision, status string) error {
		_, err := s.db.ExecContext(ctx, `UPDATE pending_actions SET target_revision=?,status=?,pending_key=NULL WHERE id=?`, revision, status, id)
		return err
	})
}
