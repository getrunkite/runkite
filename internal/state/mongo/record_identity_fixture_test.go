package mongo

import (
	"context"
	"reflect"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// Fresh test requests echo the captured fixture row or assignment identity.
// The fixed sentinel is only for transport-only or missing-record negatives;
// it never appears in production dispatch or runner authentication.
func originalRunIdentity(t testing.TB, target any, id string) string {
	t.Helper()
	switch value := target.(type) {

	case *models.Run:
		if identity := state.RunIdentity(value); identity != "" {
			return identity
		}
	case interface {
		GetRun(context.Context, string) (*models.Run, error)
	}:
		if run, err := value.GetRun(tenant.SystemContext(context.Background()), id); err == nil {
			if identity := state.RunIdentity(run); identity != "" {
				return identity
			}
		}
	case interface {
		LookupInflight(context.Context, string) (*transport.RunAssignment, error)
	}:
		if assignment, err := value.LookupInflight(context.Background(), id); err == nil && assignment != nil && assignment.RunIncarnationID != "" {
			return assignment.RunIncarnationID
		}
	}
	value := reflect.ValueOf(target)
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, name := range []string{"RunIncarnationID", "IncarnationID", "StorageIdentity"} {
			field := value.FieldByName(name)
			if field.IsValid() && field.Kind() == reflect.String && field.String() != "" {
				return field.String()
			}
		}
	}
	return "fixture-record"
}
