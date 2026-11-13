package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

// RunInsertionIdentity proves that dispatch receives the identity captured by
// the original insertion transaction, without a later GetRun race.
func RunInsertionIdentity(t *testing.T, store state.Store) {
	t.Helper()
	admitted := store.(interface {
		CreateRunAdmitted(context.Context, *models.Run, *state.RunAdmissionCaps) error
	})
	ctx := tenant.ScopedContext(t.Context(), "run-receipt-"+uuid.NewString())
	now := time.Now().UTC()
	thread := &models.Thread{ThreadID: uuid.NewString(), Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"create", "admitted"} {
		t.Run(method, func(t *testing.T) {
			row := &models.Run{RunID: uuid.NewString(), ThreadID: thread.ThreadID, AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now, IncarnationID: "untrusted-caller-identity"}
			create := func() error {
				if method == "admitted" {
					return admitted.CreateRunAdmitted(ctx, row, &state.RunAdmissionCaps{TenantConcurrent: 100})
				}
				return store.CreateRun(ctx, row)
			}
			if err := create(); err != nil {
				t.Fatal(err)
			}
			original := row.IncarnationID
			if original == "" || original == "untrusted-caller-identity" {
				t.Fatal("insertion receipt did not capture stored identity")
			}
			durable, err := store.GetRun(ctx, row.RunID)
			if err != nil || durable.IncarnationID != original {
				t.Fatalf("receipt differs from durable row: %+v %v", durable, err)
			}
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), original) || strings.Contains(string(raw), "incarnation_id") {
				t.Fatal("internal run identity leaked through client JSON")
			}
			if err := store.DeleteRun(ctx, row.RunID); err != nil {
				t.Fatal(err)
			}
			row.IncarnationID = original
			if err := create(); err != nil {
				t.Fatal(err)
			}
			if row.IncarnationID == "" || row.IncarnationID == original {
				t.Fatal("recreated run reused original insertion authority")
			}
			durable, err = store.GetRun(ctx, row.RunID)
			if err != nil || durable.IncarnationID != row.IncarnationID {
				t.Fatal("replacement receipt differs from stored run")
			}
		})
	}
}
