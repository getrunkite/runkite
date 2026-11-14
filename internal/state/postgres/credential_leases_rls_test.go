package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/tenant"
)

type credentialRLSProvider struct{ revoked int }

func (p *credentialRLSProvider) Issue(context.Context) (credentials.Grant, error) {
	value, _ := credentials.NewUserPassword("synthetic-rls-user", "synthetic-rls-password")
	return credentials.Grant{Handle: "database/creds/readonly/" + uuid.NewString(), TTL: time.Minute, Renewable: true, Credential: value}, nil
}
func (*credentialRLSProvider) Renew(_ context.Context, handle string, _ time.Duration) (credentials.Grant, error) {
	return credentials.Grant{Handle: handle, TTL: time.Minute, Renewable: true}, nil
}
func (p *credentialRLSProvider) Revoke(context.Context, string) error { p.revoked++; return nil }
func (*credentialRLSProvider) Lookup(context.Context, string) (credentials.LeaseStatus, error) {
	return credentials.LeaseStatus{Found: true, TTL: time.Minute, Renewable: true}, nil
}

func TestRLS_CredentialLifecyclePreservesDetachedTenantAndOwnership(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not configured")
	}
	s, err := New(t.Context(), dsn, WithRLS(true))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	scope := "credential-rls-" + uuid.NewString()
	ctx := tenant.ScopedContext(t.Context(), scope)
	foreign := tenant.ScopedContext(t.Context(), scope+"-foreign")
	system := tenant.SystemContext(t.Context())
	store, err := s.CredentialLeaseStorage()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := credentials.NewKeyring("fixture", map[string][]byte{"fixture": []byte(strings.Repeat("r", 32))})
	if err != nil {
		t.Fatal(err)
	}
	p := &credentialRLSProvider{}
	m, err := credentials.NewManager(store, keys, []credentials.Binding{{ID: "reporting", Revision: "rls-fixture", ConsumerID: "reporting-mcp", MaxTTL: time.Minute, RenewIncrement: 30 * time.Second, Provider: p}}, func(checkCtx context.Context, o credentials.Owner, _ string) error {
		if tenant.IsSystem(checkCtx) || tenant.FromContext(checkCtx) != o.TenantID {
			return credentials.ErrStaleOwner
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	o := credentials.Owner{TenantID: scope, RunID: uuid.NewString(), RunIncarnationID: uuid.NewString(), Generation: 1, ActionID: uuid.NewString(), ConsumerID: "reporting-mcp"}
	id, err := m.ReferenceFor(o, "reporting")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = s.pool.Exec(system, `DELETE FROM credential_leases WHERE lease_id=$1`, id) }()
	r, err := m.Execute(ctx, o, "reporting", func(consumerCtx context.Context, _ credentials.UserPassword) error {
		for _, tc := range []struct {
			ctx  context.Context
			want int
		}{{ctx, 1}, {foreign, 0}, {system, 1}} {
			var count int
			if err := s.pool.QueryRow(tc.ctx, `SELECT COUNT(*) FROM credential_leases WHERE lease_id=$1`, id).Scan(&count); err != nil || count != tc.want {
				t.Fatalf("unfiltered credential RLS count=%d want=%d err=%v", count, tc.want, err)
			}
		}
		if _, err := store.Get(foreign, scope, id); !errors.Is(err, credentials.ErrNotFound) {
			t.Fatal("foreign connection read owner-scoped credential row", err)
		}
		rows, err := store.Search(foreign, scope, 100)
		if err != nil || len(rows) != 0 {
			t.Fatal("foreign connection searched owner-scoped metadata", err)
		}
		actual, err := store.Get(consumerCtx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		actual.Version++
		if _, err := store.CAS(foreign, actual, actual.Version-1); !errors.Is(err, credentials.ErrConflict) {
			t.Fatal("foreign connection mutated owner credential row", err)
		}
		command, err := s.pool.Exec(foreign, `UPDATE credential_leases SET state='revoked' WHERE lease_id=$1`, id)
		if err != nil || command.RowsAffected() != 0 {
			t.Fatal("unfiltered foreign credential write bypassed RLS", err)
		}
		return nil
	})
	if err != nil || r == nil || r.State != credentials.StateRevoked || p.revoked != 1 {
		t.Fatal("named-tenant detached grant write or cleanup failed", err)
	}
	metadata, err := m.Get(ctx, scope, id)
	if err != nil || metadata.ProtectedHandle != "" || metadata.State != credentials.StateRevoked {
		t.Fatal("tenant metadata retained private handle or lost confirmed cleanup", err)
	}
}
