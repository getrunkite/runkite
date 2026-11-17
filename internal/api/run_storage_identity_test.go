package api

import (
	"context"
	"errors"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

type storageIdentityObservationStore struct {
	state.Store
	run *models.Run
}

func (s storageIdentityObservationStore) GetRun(context.Context, string) (*models.Run, error) {
	return s.run, nil
}

func TestInternalStorageIdentityDoesNotEnablePublicSQLReference(t *testing.T) {
	run := &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", TenantID: "default", StorageIdentity: "mongo-objectid:original"}
	if reference := runObservationReference(run); reference != "" {
		t.Fatal("internal Mongo identity enabled a public SQL reference")
	}
	latest := *run
	server := &Server{store: storageIdentityObservationStore{run: &latest}}
	ctx := tenant.WithContext(t.Context(), "default")
	if _, err := server.getObservedRun(ctx, run); err != nil {
		t.Fatal("same original record rejected", err)
	}
	latest.StorageIdentity = "mongo-objectid:replacement"
	if _, err := server.getObservedRun(ctx, run); !errors.As(err, new(*state.ErrConflict)) {
		t.Fatal("observation borrowed a replacement Mongo record", err)
	}
}
