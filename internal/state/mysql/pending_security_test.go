package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/getrunkite/runkite/internal/state/conformance"
	"github.com/getrunkite/runkite/internal/state/mysql"
)

func TestPendingSecurityLive(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN is not configured")
	}
	s, err := mysql.New(t.Context(), dsn)
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
	mutationDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mutationDB.Close()
	conformance.RunPendingTargetRevisionSecurity(t, s, func(ctx context.Context, id, revision, status string) error {
		_, err := mutationDB.ExecContext(ctx, `UPDATE pending_actions SET target_revision=?,status=?,pending_key=NULL WHERE id=?`, revision, status, id)
		return err
	})
}
