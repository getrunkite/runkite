package sqlite

import (
	"testing"

	"github.com/getrunkite/runkite/internal/state/conformance"
)

func TestConnectorActionConformance(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	conformance.RunConnectorActions(t, s)
	conformance.RunActionDeliveries(t, s)
	conformance.RunInsertionIdentity(t, s)
}
