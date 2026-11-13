package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
)

type actionUnsupportedStore struct {
	state.Store
	policy.AuditStore
}

func TestActionEvidenceRequiresAuditAndSupportedStore(t *testing.T) {
	db, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, settings, wantErr string
		store                   state.Store
	}{
		{"supported", `{"action_evidence":true,"audit_required":true}`, "", db},
		{"audit not required", `{"action_evidence":true}`, "requires policy.audit_required", db},
		{"audit disabled", `{"action_evidence":true,"audit_required":true,"audit":false}`, "requires policy.audit enabled", db},
		{"unsupported", `{"action_evidence":true,"audit_required":true}`, "connector action storage", actionUnsupportedStore{Store: db, AuditStore: db}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo":"graph.py:graph"},"policy":`+tc.settings+`}`)
			eng, _, err := initPolicy(path, tc.store, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !eng.ActionEvidenceEnabled() || !eng.AuditRequired() {
				t.Fatalf("engine=%+v err=%v", eng, err)
			}
		})
	}
}

type connectorActionMaintenanceFixture struct {
	state.Store
	state.ProtectedActionStore
	prunes, sweeps int
}

func (s *connectorActionMaintenanceFixture) MarkExpiredConnectorActionsUnknown(ctx context.Context, at time.Time, limit int) (int, error) {
	if !tenant.IsSystem(ctx) || at.IsZero() || limit != 200 {
		panic("unsafe action sweep scope")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		panic("action sweep must be bounded")
	}
	s.sweeps++
	return 0, nil
}

func (s *connectorActionMaintenanceFixture) PruneConnectorActions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if !tenant.IsSystem(ctx) || cutoff.After(time.Now().Add(-23*time.Hour)) || limit != 200 {
		panic("unsafe action retention scope")
	}
	s.prunes++
	return 0, nil
}

func TestConnectorActionMaintenanceUsesIndependentBoundedScope(t *testing.T) {
	store := &connectorActionMaintenanceFixture{}
	runConnectorActionSweepTick(tenant.WithContext(context.Background(), "tenant-a"), store)
	runRetentionTick(context.Background(), store, &retentionConfig{connectorActionsMaxAge: 24 * time.Hour})
	if store.sweeps != 1 || store.prunes != 1 {
		t.Fatalf("sweeps=%d prunes=%d", store.sweeps, store.prunes)
	}
	path := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo":"graph.py:graph"},"retention":{"connector_actions_max_age":"24h"}}`)
	got := initRetentionConfig(path)
	if got == nil || got.connectorActionsMaxAge != 24*time.Hour {
		t.Fatalf("action-only retention was ignored: %+v", got)
	}
}
