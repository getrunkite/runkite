package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type replacingThreadStore struct {
	state.Store
	replace func()
}

func (s *replacingThreadStore) CreateRun(ctx context.Context, run *models.Run) error {
	s.replace()
	return s.Store.CreateRun(ctx, run)
}

func (s *replacingThreadStore) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	s.replace()
	return s.Store.CreateRunAdmitted(ctx, run, caps)
}

func TestCreateRunRejectsThreadReplacedAfterClaim(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		name := "ordinary"
		if admitted {
			name = "admitted"
		}
		t.Run(name, func(t *testing.T) {
			store, err := sqlite.New("")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			if err := store.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			a := tenant.WithContext(context.Background(), "request-owner")
			b := tenant.WithContext(context.Background(), "replacement-owner")
			now := time.Now().UTC()
			if err := store.UpsertAgent(a, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
				t.Fatal(err)
			}
			thread := &models.Thread{ThreadID: "public-custom-id", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}
			if err := store.CreateThread(a, thread); err != nil {
				t.Fatal(err)
			}
			swapped := false
			wrapped := &replacingThreadStore{Store: store, replace: func() {
				claimed, err := store.GetThread(a, thread.ThreadID)
				if err != nil || claimed.Status != models.ThreadStatusBusy {
					t.Fatalf("replace must occur after API claim: %+v %v", claimed, err)
				}
				if err := store.DeleteThread(a, thread.ThreadID); err != nil {
					t.Fatal(err)
				}
				if err := store.CreateThread(b, thread); err != nil {
					t.Fatal(err)
				}
				swapped = true
			}}
			server := api.NewServer(wrapped, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			if admitted {
				server.SetAdmissionLimits(&api.AdmissionLimits{TenantConcurrent: 5})
			}
			request := httptest.NewRequest(http.MethodPost, "/threads/"+thread.ThreadID+"/runs", strings.NewReader(`{"agent_id":"agent","run_id":"delayed-create"}`)).WithContext(a)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if !swapped {
				t.Fatal("test did not reach the delayed insertion")
			}
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("cross-tenant parent replacement must reject create: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if _, err := store.GetRun(a, "delayed-create"); err == nil {
				t.Fatal("foreign-parent run persisted")
			}
			current, err := store.GetThread(b, thread.ThreadID)
			if err != nil || current.Status != models.ThreadStatusIdle {
				t.Fatalf("failed request changed replacement thread: %+v %v", current, err)
			}
		})
	}
}
