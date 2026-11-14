package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/getrunkite/runkite/internal/state/conformance"
)

func TestPendingSecurityLive(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not configured")
	}
	s, err := New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	conformance.RunPendingSecurity(t, s)
	conformance.RunPendingTenantScope(t, s)
	conformance.RunPendingReviewSecurity(t, s)
	conformance.RunPendingTargetRevisionSecurity(t, s, func(ctx context.Context, id, revision, status string) error {
		_, err := s.pool.Exec(ctx, `UPDATE pending_actions SET target_revision=$1,status=$2,pending_key=NULL WHERE id=$3`, revision, status, id)
		return err
	})
}
