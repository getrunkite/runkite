package conformance

import (
	"testing"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Capture fixture authority from the original stored row before admission.
// Foreign-scope tests still submit the original proof under their tested scope.
func originalRunIdentity(t *testing.T, store state.Store, id string) string {
	t.Helper()
	run, err := store.GetRun(tenant.SystemContext(t.Context()), id)
	if err != nil {
		t.Fatal(err)
	}
	identity := state.RunIdentity(run)
	if identity == "" {
		t.Fatal("fixture run has no original record identity")
	}
	return identity
}
