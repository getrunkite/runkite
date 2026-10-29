package postgres

import (
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
}
