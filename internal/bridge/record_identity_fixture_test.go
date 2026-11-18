package bridge

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

func fixtureRecordIdentity() string { return "fixture-record" }

func newIdentityServer(queue transport.JobQueue, broker transport.EventBroker, cancels transport.CancelBroker, callback func(string, string, string)) *Server {
	server := NewServer(queue, broker, cancels, callback)
	server.SetRunLifecycle(func(ctx context.Context, id, inc string, generation int64) (bool, error) {
		assignment, err := queue.LookupInflight(ctx, id)
		return assignment != nil && assignment.RunIncarnationID == inc && assignment.Generation == generation, err
	}, func(ctx context.Context, completion *state.RunCompletion) (bool, error) {
		assignment, err := queue.LookupInflight(ctx, completion.RunID)
		accepted := err == nil && assignment != nil && assignment.RunIncarnationID == completion.ExpectedIncarnationID && assignment.Generation == completion.Generation
		if accepted && callback != nil {
			callback(completion.RunID, string(completion.Status), completion.Error)
		}
		return accepted, err
	})
	return server
}

// Individual transport tests override one lifecycle step while retaining the
// other fixture guard required by the strict dispatch contract.
func setIdentityLifecycle(server *Server, attempt func(context.Context, string, string, int64) (bool, error), complete func(context.Context, *state.RunCompletion) (bool, error)) {
	if attempt == nil {
		attempt = server.attemptCallback
	}
	if complete == nil {
		complete = server.completionCallback
	}
	server.SetRunLifecycle(attempt, complete)
}
