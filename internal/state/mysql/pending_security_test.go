package mysql_test

import (
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
}
