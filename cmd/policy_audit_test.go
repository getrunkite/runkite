package main

import (
	"context"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Hide the optional AuditStore interface while retaining the state.Store
// contract, matching a compatible backend with no durable policy audit support.
type policyStoreWithoutAudit struct{ state.Store }

func TestInitPolicy_RequiredAuditBackendAndConfiguration(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, policy string
		store        state.Store
		wantRequired bool
		wantErr      string
	}{
		{name: "required supported SQL", policy: `{"default_effect":"allow","audit_required":true}`, store: store, wantRequired: true},
		{name: "required unsupported backend", policy: `{"audit_required":true}`, store: policyStoreWithoutAudit{store}, wantErr: "durable audit storage"},
		{name: "required audit disabled", policy: `{"audit_required":true,"audit":false}`, store: store, wantErr: "requires policy.audit enabled"},
		{name: "optional unsupported backend", policy: `{"default_effect":"allow"}`, store: policyStoreWithoutAudit{store}},
		{name: "optional disabled", policy: `{"audit":false}`, store: store},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo":"graph.py:graph"},"policy":`+tc.policy+`}`)
			eng, runEvents, err := initPolicy(path, tc.store, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || eng != nil {
					t.Fatalf("engine = %v, error = %v, want %q", eng, err, tc.wantErr)
				}
				return
			}
			if err != nil || eng == nil || eng.AuditRequired() != tc.wantRequired || !runEvents {
				t.Fatalf("engine = %v, runEvents = %v, error = %v", eng, runEvents, err)
			}
			if tc.wantRequired {
				ctx := tenant.WithContext(context.Background(), "acme")
				dec := eng.Decide(ctx, policy.PolicyInput{Stage: policy.StageToolCall, TenantID: "acme", AgentID: "support", Connector: "crm", Tool: "query"})
				if dec.Effect != policy.EffectAllow || dec.AuditFailed {
					t.Fatalf("valid durable audit did not allow: %#v", dec)
				}
				rows, err := store.SearchAuditEvents(ctx, &models.AuditSearchRequest{Limit: 10})
				if err != nil || len(rows) != 1 || rows[0].Decision != policy.EffectAllow {
					t.Fatalf("durable audit rows = %#v, error = %v", rows, err)
				}
			}
		})
	}
}

func TestInitPolicy_ConfigurationErrorsCannotDisablePolicy(t *testing.T) {
	path := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo":"graph.py:graph"},"policy":{"audit_required":"true"}}`)
	eng, _, err := initPolicy(path, nil, nil)
	if err == nil || eng != nil {
		t.Fatalf("invalid policy silently disabled protection: engine = %v, error = %v", eng, err)
	}
	path = writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo":"graph.py:graph"}}`)
	eng, runEvents, err := initPolicy(path, nil, nil)
	if err != nil || eng != nil || runEvents {
		t.Fatalf("absent policy changed compatibility: engine = %v, runEvents = %v, error = %v", eng, runEvents, err)
	}
}
