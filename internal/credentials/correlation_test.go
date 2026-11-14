package credentials

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestVaultCorrelationUsesServerOwnerAndSafeRequestIDs(t *testing.T) {
	const issueID = "646529cb-e785-4bf2-900b-617eb8ab5463"
	const renewID = "8475c529-2c17-42fe-b859-146210e85d47"
	const lease = "database/creds/readonly/synthetic-lease"
	provider, _ := fixtureVault(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Runkite-Action-Id") != "action" || r.Header.Get("X-Runkite-Lease-Ref") != "cred-synthetic" {
			t.Error("trusted correlation absent")
		}
		switch r.URL.Path {
		case "/v1/database/creds/readonly":
			_, _ = w.Write([]byte(`{"request_id":"` + issueID + `","lease_id":"` + lease + `","lease_duration":30,"renewable":true,"data":{"username":"synthetic-user","password":"synthetic-secret"}}`))
		case "/v1/sys/leases/renew":
			_, _ = w.Write([]byte(`{"request_id":"` + renewID + `","lease_id":"` + lease + `","lease_duration":30,"renewable":true}`))
		case "/v1/sys/leases/revoke":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected provider request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	ctx := withProviderCorrelation(t.Context(), fixtureOwner(), "cred-synthetic")
	g, err := provider.Issue(ctx)
	if err != nil || g.RequestID != issueID {
		t.Fatalf("issue correlation: %s %v", g.RequestID, err)
	}
	g, err = provider.Renew(ctx, lease, time.Second)
	if err != nil || g.RequestID != renewID {
		t.Fatalf("renew correlation: %s %v", g.RequestID, err)
	}
	if err := provider.Revoke(ctx, lease); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"synthetic-secret", "not-a-uuid", strings.Repeat("a", 1024), "00000000-0000-0000-0000-000000000000", strings.ToUpper(issueID)} {
		if safeVaultRequestID(bad) != "" {
			t.Fatal("arbitrary provider text accepted as safe correlation")
		}
	}
	h := http.Header{}
	addProviderCorrelation(withProviderCorrelation(t.Context(), Owner{ActionID: "bad\r\nheader"}, "cred-synthetic"), h)
	if len(h) != 0 {
		t.Fatal("unsafe correlation header accepted")
	}
}

type correlatedFixtureProvider struct {
	*fakeProvider
	issueID, renewID string
}

func (p *correlatedFixtureProvider) Issue(ctx context.Context) (Grant, error) {
	g, e := p.fakeProvider.Issue(ctx)
	g.RequestID = p.issueID
	return g, e
}
func (p *correlatedFixtureProvider) Renew(ctx context.Context, h string, d time.Duration) (Grant, error) {
	g, e := p.fakeProvider.Renew(ctx, h, d)
	g.RequestID = p.renewID
	return g, e
}

func TestCredentialCorrelationSurvivesLifecycleAndSQLRoundtrip(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &correlatedFixtureProvider{fakeProvider: &fakeProvider{ttl: 45 * time.Second, renewTTL: 30 * time.Second}, issueID: "646529cb-e785-4bf2-900b-617eb8ab5463", renewID: "8475c529-2c17-42fe-b859-146210e85d47"}
	m := fixtureManager(t, store, p, nil)
	owner := fixtureOwner()
	record, err := m.Execute(t.Context(), owner, "reporting", func(ctx context.Context, _ UserPassword) error {
		rows, e := store.ListUnresolved(ctx, 10)
		if e != nil || len(rows) != 1 {
			t.Fatalf("lease unavailable: %v", e)
		}
		renewed, e := m.Renew(ctx, owner, rows[0].ID)
		if e != nil {
			t.Fatal(e)
		}
		if renewed.IssueRequestID != p.issueID || renewed.LastRenewRequestID != p.renewID {
			t.Fatal("renewal lost correlation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := m.Get(t.Context(), owner.TenantID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IssueRequestID != p.issueID || stored.LastRenewRequestID != p.renewID || stored.State != StateRevoked {
		t.Fatal("cleanup/SQL lost correlation")
	}
	raw, _ := json.Marshal(stored)
	for _, secret := range []string{"synthetic-user", "synthetic-password", "database/creds/readonly/"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret or raw cleanup handle leaked")
		}
	}
}
