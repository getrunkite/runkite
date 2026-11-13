package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/getrunkite/runkite/internal/tenant"
)

type fakeProvider struct {
	mu                               sync.Mutex
	issued, renewed, revoked, looked int
	ttl, renewTTL                    time.Duration
	issueErr, revokeErr              error
	hook                             func()
	leases                           map[string]bool
}

func (p *fakeProvider) Issue(context.Context) (Grant, error) {
	p.mu.Lock()
	p.issued++
	hook, err, ttl := p.hook, p.issueErr, p.ttl
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return Grant{}, err
	}
	handle := "database/creds/readonly/" + uuid.NewString()
	p.mu.Lock()
	if p.leases == nil {
		p.leases = map[string]bool{}
	}
	p.leases[handle] = true
	p.mu.Unlock()
	return Grant{Handle: handle, TTL: ttl, Renewable: true, Credential: UserPassword{"synthetic-user", "synthetic-password"}}, nil
}
func (p *fakeProvider) Renew(_ context.Context, handle string, _ time.Duration) (Grant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.renewed++
	return Grant{Handle: handle, TTL: p.renewTTL, Renewable: true}, nil
}
func (p *fakeProvider) Revoke(_ context.Context, handle string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked++
	if p.revokeErr != nil {
		return p.revokeErr
	}
	delete(p.leases, handle)
	return nil
}
func (p *fakeProvider) Lookup(_ context.Context, handle string) (LeaseStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.looked++
	return LeaseStatus{Found: p.leases[handle], TTL: p.ttl, Renewable: true}, nil
}

func fixtureStore(t *testing.T) (*SQLStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	stmts, err := Schema(SQLite)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewSQLStore(db, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}
func fixtureOwner() Owner { return Owner{"acme", "run", "incarnation", 1, "action", "reporting-mcp"} }
func fixtureManager(t *testing.T, store Store, p Provider, check ActiveChecker) *Manager {
	t.Helper()
	keys, err := NewKeyring("first", map[string][]byte{"first": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if check == nil {
		check = func(context.Context, Owner, string) error { return nil }
	}
	m, err := NewManager(store, keys, []Binding{{ID: "reporting", Revision: "revision-1", ConsumerID: "reporting-mcp", MaxTTL: time.Minute, RenewIncrement: 30 * time.Second, Provider: p}}, check)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestExecutePersistsOnlyProtectedMetadataAndNeverReplays(t *testing.T) {
	store, db := fixtureStore(t)
	p := &fakeProvider{ttl: 45 * time.Second}
	m := fixtureManager(t, store, p, nil)
	o := fixtureOwner()
	calls := 0
	r, err := m.Execute(t.Context(), o, "reporting", func(ctx context.Context, u UserPassword) error {
		calls++
		user, password := u.Values()
		if user != "synthetic-user" || password != "synthetic-password" {
			t.Fatal("consumer did not get the ephemeral database credential")
		}
		rows, err := store.ListUnresolved(ctx, 10)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		record := rows[0]
		if record.State != StateActive || record.UseStartedAt == nil || record.GrantedTTLSeconds != 45 {
			t.Fatalf("use lacked durable grant: %s", record)
		}
		rawHandle, err := m.keys.open(record)
		if err != nil {
			t.Fatal(err)
		}
		var metadata, protected string
		if err := db.QueryRow(`SELECT record_json,protected_handle FROM credential_leases`).Scan(&metadata, &protected); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{user, password, rawHandle} {
			if strings.Contains(metadata, secret) || strings.Contains(protected, secret) {
				t.Fatal("credential or provider handle persisted in plaintext")
			}
		}
		encoded, _ := json.Marshal(record)
		if strings.Contains(string(encoded), protected) || strings.Contains(fmt.Sprintf("%+v %#v", record, record), protected) {
			t.Fatal("protected handle leaked to metadata output")
		}
		for _, v := range []any{u, Grant{Handle: rawHandle, Credential: u}} {
			encoded, _ := json.Marshal(v)
			if string(encoded) != "{}" || strings.Contains(fmt.Sprintf("%+v %#v", v, v), password) {
				t.Fatal("ephemeral credential is printable")
			}
		}
		return nil
	})
	if err != nil || r.State != StateRevoked || r.ProtectedHandle != "" || calls != 1 || p.issued != 1 || p.revoked != 1 {
		t.Fatalf("unexpected lifecycle: %s %v calls=%d", r, err, calls)
	}
	_, err = m.Execute(t.Context(), o, "reporting", func(context.Context, UserPassword) error { calls++; return nil })
	if !errors.Is(err, ErrAlreadyIssued) || p.issued != 1 || calls != 1 {
		t.Fatal("replay issued or consumed another credential")
	}
}

func TestUnknownIssuanceSurvivesRestartWithoutRetry(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{issueErr: errors.New("synthetic-network-failure")}
	m := fixtureManager(t, store, p, nil)
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { t.Fatal("unknown credential reached consumer"); return nil })
	if !errors.Is(err, ErrIssuanceUnknown) || r.State != StateIssuanceUnknown || r.ProtectedHandle != "" {
		t.Fatalf("lost issuance misrepresented: %s %v", r, err)
	}
	restarted := fixtureManager(t, store, p, nil)
	if _, err := restarted.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { return nil }); !errors.Is(err, ErrAlreadyIssued) {
		t.Fatal(err)
	}
	if _, err := restarted.Reconcile(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if p.issued != 1 || p.looked != 0 || p.revoked != 0 {
		t.Fatal("lost handle was guessed or issuance was replayed")
	}
}

func TestConcurrentReplicasOnlyIssueOnce(t *testing.T) {
	store, _ := fixtureStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p := &fakeProvider{ttl: time.Minute, hook: func() { close(entered); <-release }}
	m1, m2 := fixtureManager(t, store, p, nil), fixtureManager(t, store, p, nil)
	done := make(chan error, 1)
	go func() {
		_, err := m1.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { return nil })
		done <- err
	}()
	<-entered
	if _, err := m2.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { t.Error("second consumer executed"); return nil }); !errors.Is(err, ErrAlreadyIssued) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.issued != 1 {
		t.Fatal("duplicate issuance")
	}
}

func TestReclaimBetweenIssuanceAndUseCannotReleaseCredential(t *testing.T) {
	store, _ := fixtureStore(t)
	var active atomic.Bool
	active.Store(true)
	p := &fakeProvider{ttl: time.Minute, hook: func() { active.Store(false) }}
	m := fixtureManager(t, store, p, func(context.Context, Owner, string) error {
		if !active.Load() {
			return ErrStaleOwner
		}
		return nil
	})
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error {
		t.Fatal("superseded owner received credentials")
		return nil
	})
	if !errors.Is(err, ErrStaleOwner) || r.State != StateRevoked || p.revoked != 1 {
		t.Fatalf("old lease not cleaned: %s %v", r, err)
	}
}

func TestGrantedTTLAndOldOwnerRenewalFencing(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute, renewTTL: 17 * time.Second}
	m := fixtureManager(t, store, p, nil)
	o := fixtureOwner()
	_, err := m.Execute(t.Context(), o, "reporting", func(ctx context.Context, _ UserPassword) error {
		rows, _ := store.ListUnresolved(ctx, 10)
		lease := rows[0]
		foreign := o
		foreign.Generation++
		if _, err := m.Renew(ctx, foreign, lease.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("foreign generation renewed current lease", err)
		}
		if _, err := m.Revoke(ctx, foreign, lease.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("foreign generation revoked current lease", err)
		}
		renewed, err := m.Renew(ctx, o, lease.ID)
		if err != nil || renewed.GrantedTTLSeconds != 17 || renewed.ExpiresAt.Sub(renewed.UpdatedAt) > 18*time.Second {
			t.Fatalf("requested increment was treated as returned TTL: %s %v", renewed, err)
		}
		return nil
	})
	if err != nil || p.renewed != 1 {
		t.Fatal(err)
	}
}

func TestRevocationUnknownIsNotConfirmationAndReconciles(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute, revokeErr: errors.New("synthetic-provider-outage")}
	m := fixtureManager(t, store, p, nil)
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { return nil })
	if !errors.Is(err, ErrCleanupPending) || r.State != StateRevocationUnknown || r.ProtectedHandle == "" {
		t.Fatalf("revocation falsely confirmed: %s %v", r, err)
	}
	p.revokeErr = nil
	if count, err := m.Reconcile(t.Context(), 10); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	actual, _ := store.Get(t.Context(), r.Owner.TenantID, r.ID)
	if actual.State != StateRevoked {
		t.Fatal(actual)
	}
}

func TestOversizedProviderTTLNeverReachesConsumer(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: 2 * time.Minute}
	m := fixtureManager(t, store, p, nil)
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { t.Fatal("out-of-contract credential used"); return nil })
	if err == nil || r.State != StateRevoked || p.revoked != 1 {
		t.Fatalf("oversized provider grant not cleaned: %s %v", r, err)
	}
}

func TestConsumerFailureAndPanicStillCleanWithoutErrorSecret(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprint(panics), func(t *testing.T) {
			store, _ := fixtureStore(t)
			p := &fakeProvider{ttl: time.Minute}
			m := fixtureManager(t, store, p, nil)
			func() {
				if panics {
					defer func() {
						if recover() == nil {
							t.Error("callback panic disappeared")
						}
					}()
				}
				_, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error {
					if panics {
						panic("synthetic callback")
					}
					return errors.New("synthetic-password")
				})
				if !panics && (!errors.Is(err, ErrConsumerFailed) || strings.Contains(err.Error(), "synthetic-password")) {
					t.Fatal("consumer error exposed secrets", err)
				}
			}()
			if p.revoked != 1 {
				t.Fatal("consumer failure skipped cleanup")
			}
		})
	}
}

func seedActive(t *testing.T, m *Manager, o Owner, binding Binding, handle string) *Record {
	t.Helper()
	now := m.now()
	r, created, err := m.store.Create(t.Context(), &Record{ID: uuid.NewString(), Owner: o, BindingID: binding.ID, BindingRevision: binding.Revision, State: StateIssuing, Version: 1, CreatedAt: now, UpdatedAt: now, OperationDeadline: now.Add(operationTimeout)})
	if err != nil || !created {
		t.Fatal(err)
	}
	r.State = StateActive
	r.ExpiresAt = now.Add(time.Minute)
	r.Renewable = true
	r.GrantedTTLSeconds = 60
	r.ProtectedHandle, err = m.keys.seal(r, handle)
	if err != nil {
		t.Fatal(err)
	}
	r, err = m.save(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProtectedHandleCannotMoveAcrossOwnerAndTenant(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, store, p, nil)
	r := seedActive(t, m, fixtureOwner(), m.active["reporting"], "database/creds/readonly/synthetic-private-handle")
	if _, err := store.Get(t.Context(), "ACME", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("tenant identity widened", err)
	}
	for _, change := range []func(*Record){func(r *Record) { r.Owner.Generation++ }, func(r *Record) { r.Owner.TenantID = "other" }, func(r *Record) { r.Owner.RunIncarnationID = "replacement" }, func(r *Record) { r.Owner.ActionID = "another-action" }, func(r *Record) { r.BindingRevision = "revision-2" }} {
		n := clone(r)
		change(n)
		if _, err := m.keys.open(n); err == nil {
			t.Fatal("protected handle authenticated under a different owner")
		}
		n.Version++
		if _, err := store.CAS(t.Context(), n, r.Version); err == nil {
			t.Fatal("store permitted immutable identity replacement")
		}
	}
}

func TestReconcileOldAttemptWithoutRevokingReplacementAndRetainProviderRevision(t *testing.T) {
	store, _ := fixtureStore(t)
	old := &fakeProvider{ttl: time.Minute, leases: map[string]bool{"database/creds/readonly/old": true}}
	m := fixtureManager(t, store, old, nil)
	owner := fixtureOwner()
	r := seedActive(t, m, owner, m.active["reporting"], "database/creds/readonly/old")
	newProvider := &fakeProvider{ttl: time.Minute}
	next := owner
	next.Generation++
	next.ActionID = "replacement-action"
	m.revisions["revision-1"] = Binding{ID: "reporting", Revision: "revision-1", ConsumerID: owner.ConsumerID, MaxTTL: time.Minute, RenewIncrement: time.Second, Provider: old, Retired: true}
	m.revisions["revision-2"] = Binding{ID: "reporting", Revision: "revision-2", ConsumerID: owner.ConsumerID, MaxTTL: time.Minute, RenewIncrement: time.Second, Provider: newProvider}
	m.active["reporting"] = m.revisions["revision-2"]
	m.check = func(_ context.Context, o Owner, _ string) error {
		if o.Generation != next.Generation {
			return ErrStaleOwner
		}
		return nil
	}
	if _, err := m.Renew(t.Context(), owner, r.ID); !errors.Is(err, ErrStaleOwner) {
		t.Fatal("old attempt renewed", err)
	}
	if _, err := m.Reconcile(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if old.revoked != 1 || newProvider.revoked != 0 {
		t.Fatal("cleanup used replacement provider configuration")
	}
}

func TestBoundedReconcileCannotStarveNewerAbandonedLease(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute, leases: map[string]bool{"database/creds/readonly/old": true, "database/creds/readonly/abandoned": true}}
	m := fixtureManager(t, store, p, nil)
	first := fixtureOwner()
	_ = seedActive(t, m, first, m.active["reporting"], "database/creds/readonly/old")
	second := first
	second.ActionID = "later-action"
	r := seedActive(t, m, second, m.active["reporting"], "database/creds/readonly/abandoned")
	r.CleanupRequested = true
	if _, err := m.save(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Reconcile(t.Context(), 1); err != nil || n != 1 {
		t.Fatal("bounded scan starved cleanup", n, err)
	}
}

type interceptStore struct {
	Store
	after         func(*Record)
	loseActiveAck bool
}

func (s *interceptStore) CAS(ctx context.Context, r *Record, expected int64) (*Record, error) {
	actual, err := s.Store.CAS(ctx, r, expected)
	if err != nil {
		return nil, err
	}
	if s.after != nil {
		s.after(actual)
	}
	if s.loseActiveAck && r.State == StateActive && r.UseStartedAt == nil {
		s.loseActiveAck = false
		return nil, ErrUnavailable
	}
	return actual, nil
}

func TestCommittedGrantAcknowledgementDoesNotIssueTwice(t *testing.T) {
	store, _ := fixtureStore(t)
	intercept := &interceptStore{Store: store, loseActiveAck: true}
	p := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, intercept, p, nil)
	called := 0
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error { called++; return nil })
	if err != nil || called != 1 || p.issued != 1 || r.State != StateRevoked {
		t.Fatalf("commit acknowledgement recovery failed: %s %v", r, err)
	}
}

func TestReclaimDuringUseWriteStopsBeforeCallback(t *testing.T) {
	store, _ := fixtureStore(t)
	var active atomic.Bool
	active.Store(true)
	intercept := &interceptStore{Store: store, after: func(r *Record) {
		if r.UseStartedAt != nil {
			active.Store(false)
		}
	}}
	p := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, intercept, p, func(context.Context, Owner, string) error {
		if !active.Load() {
			return ErrStaleOwner
		}
		return nil
	})
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(context.Context, UserPassword) error {
		t.Fatal("post-write stale owner received credential")
		return nil
	})
	if !errors.Is(err, ErrStaleOwner) || r.State != StateRevoked || p.revoked != 1 {
		t.Fatal("post-write stale lease not cleaned", err)
	}
}

func TestMetadataSearchIsTenantScopedAndStripsProtectedHandle(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, store, p, nil)
	o := fixtureOwner()
	first := seedActive(t, m, o, m.active["reporting"], "database/creds/readonly/first-private-handle")
	o.TenantID = "another-tenant"
	o.ActionID = "newer-action"
	_ = seedActive(t, m, o, m.active["reporting"], "database/creds/readonly/second-private-handle")
	rows, err := m.Search(t.Context(), "acme", 1)
	if err != nil || len(rows) != 1 || rows[0].ID != first.ID || rows[0].ProtectedHandle != "" {
		t.Fatal("newer foreign row hid or exposed tenant record", err)
	}
	r, err := m.Get(t.Context(), "acme", first.ID)
	if err != nil || r.ProtectedHandle != "" {
		t.Fatal("metadata get exposed private handle", err)
	}
	if _, err := m.Get(t.Context(), "ACME", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("metadata get widened tenant identity", err)
	}
}

func TestConsumerDeadlineUsesActualGrantAndCleanupStillRuns(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Second}
	m := fixtureManager(t, store, p, nil)
	r, err := m.Execute(t.Context(), fixtureOwner(), "reporting", func(ctx context.Context, _ UserPassword) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 950*time.Millisecond {
			t.Fatal("consumer lacked a deadline before actual grant expiry")
		}
		<-ctx.Done()
		return nil
	})
	if !errors.Is(err, ErrConsumerFailed) || r.State != StateRevoked || p.revoked != 1 {
		t.Fatal("late consumer success or missing cleanup", err)
	}
}

func TestMissingHistoricalProviderCannotStarveNewerCleanup(t *testing.T) {
	store, _ := fixtureStore(t)
	p := &fakeProvider{ttl: time.Minute, leases: map[string]bool{"database/creds/readonly/old": true, "database/creds/readonly/new": true}}
	m := fixtureManager(t, store, p, nil)
	first := seedActive(t, m, fixtureOwner(), m.active["reporting"], "database/creds/readonly/old")
	first.CleanupRequested = true
	if _, err := m.save(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	b := m.active["reporting"]
	b.Revision = "new-revision"
	m.active["reporting"] = b
	m.revisions[b.Revision] = b
	delete(m.revisions, "revision-1")
	o := fixtureOwner()
	o.ActionID = "new-action"
	second := seedActive(t, m, o, b, "database/creds/readonly/new")
	second.CleanupRequested = true
	if _, err := m.save(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(t.Context(), 1); !errors.Is(err, ErrCleanupPending) {
		t.Fatal("missing provider was hidden", err)
	}
	if n, err := m.Reconcile(t.Context(), 1); err != nil || n != 1 {
		t.Fatal("unrecoverable historical row starved healthy cleanup", n, err)
	}
	unknown, err := m.Get(t.Context(), first.Owner.TenantID, first.ID)
	if err != nil || unknown.State == StateRevoked {
		t.Fatal("missing provider was falsely cleaned", err)
	}
}

func TestRetiredBindingIsCleanupOnlyEvenWhenOwnerRemainsActive(t *testing.T) {
	store, _ := fixtureStore(t)
	old := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, store, old, nil)
	r := seedActive(t, m, fixtureOwner(), m.active["reporting"], "database/creds/readonly/old")
	retired := m.active["reporting"]
	retired.Retired = true
	m.revisions[retired.Revision] = retired
	newProvider := &fakeProvider{ttl: time.Minute}
	next := retired
	next.Retired = false
	next.Revision = "new-role"
	next.Provider = newProvider
	m.active["reporting"] = next
	m.revisions[next.Revision] = next
	if _, err := m.Renew(t.Context(), r.Owner, r.ID); !errors.Is(err, ErrStaleOwner) || old.renewed != 0 || newProvider.renewed != 0 {
		t.Fatal("retired role lease was renewed", err)
	}
	if _, err := m.Revoke(t.Context(), r.Owner, r.ID); err != nil || old.revoked != 1 || newProvider.revoked != 0 {
		t.Fatal("retired lease cleanup used replacement provider", err)
	}
}

type scopeCheckingStore struct{ Store }

func (s scopeCheckingStore) Get(ctx context.Context, scope, id string) (*Record, error) {
	if tenant.IsSystem(ctx) || tenant.FromContext(ctx) != scope {
		return nil, ErrUnavailable
	}
	return s.Store.Get(ctx, scope, id)
}
func (s scopeCheckingStore) CAS(ctx context.Context, r *Record, expected int64) (*Record, error) {
	if tenant.IsSystem(ctx) || tenant.FromContext(ctx) != r.Owner.TenantID {
		return nil, ErrUnavailable
	}
	return s.Store.CAS(ctx, r, expected)
}

func TestDetachedGrantAndCleanupPreserveTrustedTenantScope(t *testing.T) {
	store, _ := fixtureStore(t)
	scoped := scopeCheckingStore{store}
	p := &fakeProvider{ttl: time.Minute}
	m := fixtureManager(t, scoped, p, nil)
	r, err := m.Execute(tenant.ScopedContext(t.Context(), "acme"), fixtureOwner(), "reporting", func(ctx context.Context, _ UserPassword) error {
		if tenant.FromContext(ctx) != "acme" {
			t.Fatal("consumer scope lost")
		}
		return nil
	})
	if err != nil || r.State != StateRevoked || p.revoked != 1 {
		t.Fatal("detached lifecycle lost named-tenant RLS context", err)
	}
}
