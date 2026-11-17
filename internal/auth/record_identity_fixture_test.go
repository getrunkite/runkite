package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
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

func withFixtureHeaderProof(t testing.TB, target any, headers http.Header, name, value string) string {
	id := value
	if strings.EqualFold(name, "X-Runkite-Run-Id-B64") {
		if raw, err := base64.RawURLEncoding.DecodeString(value); err == nil {
			id = string(raw)
		}
	}
	if headers.Get("X-Runkite-Run-Incarnation") == "" {
		headers.Set("X-Runkite-Run-Incarnation", originalRunIdentity(t, target, id))
	}
	return value
}
