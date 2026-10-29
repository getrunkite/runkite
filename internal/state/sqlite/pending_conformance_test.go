package sqlite

import (
	"github.com/getrunkite/runkite/internal/state/conformance"
	"testing"
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
}
