package credentials

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/getrunkite/runkite/internal/tenant"
)

const operationTimeout = 15 * time.Second

type Manager struct {
	store     Store
	keys      *Keyring
	active    map[string]Binding
	revisions map[string]Binding
	check     ActiveChecker
	now       func() time.Time
}

func NewManager(store Store, keys *Keyring, bindings []Binding, check ActiveChecker) (*Manager, error) {
	if store == nil || keys == nil || check == nil || len(bindings) == 0 {
		return nil, ErrUnavailable
	}
	m := &Manager{store: store, keys: keys, active: map[string]Binding{}, revisions: map[string]Binding{}, check: check, now: func() time.Time { return time.Now().UTC() }}
	for _, b := range bindings {
		if b.ID == "" || b.Revision == "" || b.ConsumerID == "" || b.Provider == nil || b.MaxTTL < time.Second || b.MaxTTL > 24*time.Hour || b.RenewIncrement < time.Second || b.RenewIncrement > b.MaxTTL {
			return nil, ErrUnavailable
		}
		if _, ok := m.revisions[b.Revision]; ok {
			return nil, ErrUnavailable
		}
		m.revisions[b.Revision] = b
		if !b.Retired {
			if _, ok := m.active[b.ID]; ok {
				return nil, ErrUnavailable
			}
			m.active[b.ID] = b
		}
	}
	return m, nil
}

// BindingRevision lets the connector target contract include the immutable
// registered provider role, rather than trusting a mutable binding ID alone.
func (m *Manager) BindingRevision(bindingID string) (string, error) {
	b, ok := m.active[bindingID]
	if !ok {
		return "", ErrUnavailable
	}
	return b.Revision, nil
}

// ReferenceFor permits action correlation before policy without issuing or
// fetching any credential. ActionID is a server-generated random request ID.
func (m *Manager) ReferenceFor(o Owner, bindingID string) (string, error) {
	b, ok := m.active[bindingID]
	if !ok || !o.valid() || b.ConsumerID != o.ConsumerID {
		return "", ErrUnavailable
	}
	return "cred-" + ownerKey(o, bindingID)[:56], nil
}

func (m *Manager) Get(ctx context.Context, tenant, id string) (*Record, error) {
	r, err := m.store.Get(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	r = clone(r)
	r.ProtectedHandle = ""
	return r, nil
}

func (m *Manager) Search(ctx context.Context, tenant string, limit int) ([]*Record, error) {
	if tenant == "" {
		return nil, ErrUnavailable
	}
	rows, err := m.store.Search(ctx, tenant, limit)
	if err != nil {
		return nil, err
	}
	result := make([]*Record, 0, len(rows))
	for _, r := range rows {
		if r.Owner.TenantID != tenant {
			return nil, ErrUnavailable
		}
		r = clone(r)
		r.ProtectedHandle = ""
		result = append(result, r)
	}
	return result, nil
}

func (m *Manager) binding(r *Record) (Binding, error) {
	b, ok := m.revisions[r.BindingRevision]
	if !ok || b.ID != r.BindingID || b.ConsumerID != r.Owner.ConsumerID {
		return Binding{}, ErrUnavailable
	}
	return b, nil
}

func (m *Manager) owned(ctx context.Context, o Owner, id string) (*Record, error) {
	if !o.valid() {
		return nil, ErrNotFound
	}
	r, err := m.store.Get(ctx, o.TenantID, id)
	if err != nil {
		return nil, err
	}
	if r.Owner != o {
		return nil, ErrNotFound
	}
	return r, nil
}

func (m *Manager) save(ctx context.Context, next *Record) (*Record, error) {
	expected := next.Version
	next = clone(next)
	next.Version++
	next.UpdatedAt = m.now()
	r, err := m.store.CAS(ctx, next, expected)
	if err == nil {
		return r, nil
	}
	// A committed mutation may have lost its acknowledgement. A proven exact
	// replay is metadata recovery only, never a reason to repeat provider work.
	if actual, getErr := m.store.Get(ctx, next.Owner.TenantID, next.ID); getErr == nil && reflect.DeepEqual(actual, next) {
		return actual, nil
	}
	return nil, err
}

func (m *Manager) cleanupDetached(o Owner, id string) (*Record, error) {
	ctx, cancel := context.WithTimeout(tenant.ScopedContext(context.Background(), o.TenantID), operationTimeout)
	defer cancel()
	return m.Revoke(ctx, o, id)
}

func (m *Manager) saveDetached(r *Record) (*Record, error) {
	ctx, cancel := context.WithTimeout(tenant.ScopedContext(context.Background(), r.Owner.TenantID), operationTimeout)
	defer cancel()
	return m.save(ctx, r)
}

func (m *Manager) getDetached(tenantID, id string) (*Record, error) {
	ctx, cancel := context.WithTimeout(tenant.ScopedContext(context.Background(), tenantID), operationTimeout)
	defer cancel()
	return m.store.Get(ctx, tenantID, id)
}

// Execute releases credentials only after durable issuance and one-time use
// records and two current-authority checks. The callback is a trusted consumer,
// never arbitrary agent code. It is called at most once for this owner/binding.
func (m *Manager) Execute(ctx context.Context, o Owner, bindingID string, consume func(context.Context, UserPassword) error) (*Record, error) {
	b, ok := m.active[bindingID]
	if !ok || !o.valid() || b.ConsumerID != o.ConsumerID || consume == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, ErrUnavailable
	}
	if err := m.check(ctx, o, bindingID); err != nil {
		return nil, ErrStaleOwner
	}
	started := m.now()
	id, err := m.ReferenceFor(o, bindingID)
	if err != nil {
		return nil, err
	}
	r := &Record{ID: id, Owner: o, BindingID: bindingID, BindingRevision: b.Revision, State: StateIssuing, Version: 1, CreatedAt: started, UpdatedAt: started, OperationDeadline: started.Add(operationTimeout)}
	r, created, err := m.store.Create(ctx, r)
	if err != nil {
		return nil, ErrUnavailable
	}
	if !created {
		return r, ErrAlreadyIssued
	}
	g, issueErr := b.Provider.Issue(ctx)
	if g.Handle == "" {
		n := clone(r)
		n.State = StateIssuanceUnknown
		n.CleanupRequested = true
		n.ReasonCode = "issuance_outcome_unknown"
		var pe *providerError
		if errors.As(issueErr, &pe) && pe.notSent {
			n.State = StateNotIssued
			n.ReasonCode = "issuance_not_sent"
		}
		if stored, e := m.saveDetached(n); e == nil {
			r = stored
		}
		return r, ErrIssuanceUnknown
	}
	protected, err := m.keys.seal(r, g.Handle)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		_ = b.Provider.Revoke(cleanupCtx, g.Handle)
		cancel()
		return r, ErrUnavailable
	}
	n := clone(r)
	n.ProtectedHandle = protected
	n.State = StateActive
	n.GrantedTTLSeconds = int64(g.TTL / time.Second)
	n.ExpiresAt = started.Add(g.TTL)
	n.Renewable = g.Renewable
	if issueErr != nil || g.TTL < time.Second || g.TTL > b.MaxTTL || !n.ExpiresAt.After(m.now()) {
		n.CleanupRequested = true
		n.ReasonCode = "provider_grant_outside_contract"
	}
	if g.Credential.username == "" || g.Credential.password == "" {
		n.CleanupRequested = true
		n.ReasonCode = "provider_credential_unavailable"
	}
	r, err = m.saveDetached(n)
	if err != nil {
		// Save the late known handle into a recovered intent if reconciliation
		// already marked it unknown. Never activate or use this credential.
		if actual, e := m.getDetached(o.TenantID, n.ID); e == nil && actual.Owner == o && actual.ProtectedHandle == "" {
			actual.ProtectedHandle = protected
			actual.CleanupRequested = true
			actual.State = StateRevocationUnknown
			actual.ReasonCode = "issuance_commit_unconfirmed"
			if stored, e := m.saveDetached(actual); e == nil {
				r = stored
			}
		}
		if r != nil && r.ProtectedHandle != "" {
			_, _ = m.cleanupDetached(o, n.ID)
		} else {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), operationTimeout)
			_ = b.Provider.Revoke(cleanupCtx, g.Handle)
			cancel()
		}
		return r, ErrIssuanceUnknown
	}
	if r.CleanupRequested {
		cleaned, _ := m.cleanupDetached(o, r.ID)
		if cleaned != nil {
			r = cleaned
		}
		return r, ErrUnavailable
	}
	if ctx.Err() != nil || m.check(ctx, o, bindingID) != nil {
		cleaned, _ := m.cleanupDetached(o, r.ID)
		if cleaned != nil {
			r = cleaned
		}
		return r, ErrStaleOwner
	}
	n = clone(r)
	at := m.now()
	n.UseStartedAt = &at
	r, err = m.save(ctx, n)
	if err != nil {
		_, _ = m.cleanupDetached(o, n.ID)
		return n, ErrUnavailable
	}
	// A cancellation or reclaim during the durable write cannot release the
	// credential just because the earlier check passed.
	if ctx.Err() != nil || m.check(ctx, o, bindingID) != nil {
		cleaned, _ := m.cleanupDetached(o, r.ID)
		if cleaned != nil {
			r = cleaned
		}
		return r, ErrStaleOwner
	}
	// A short actual grant cannot authorize an arbitrarily long consumer call.
	// This context is passed to the trusted HTTP/database consumer. Renewal is
	// explicit and never silently extends an already issued callback deadline.
	margin := min(time.Second, g.TTL/10)
	useCtx, cancelUse := context.WithDeadline(ctx, r.ExpiresAt.Add(-margin))
	defer cancelUse()
	if useCtx.Err() != nil {
		cleaned, _ := m.cleanupDetached(o, r.ID)
		if cleaned != nil {
			r = cleaned
		}
		return r, ErrUnavailable
	}
	var consumerErr, cleanupErr error
	var cleaned *Record
	func() {
		defer func() { cleaned, cleanupErr = m.cleanupDetached(o, r.ID) }()
		consumerErr = consume(useCtx, g.Credential)
		if consumerErr == nil && useCtx.Err() != nil {
			consumerErr = ErrUnavailable
		}
	}()
	if cleaned != nil {
		r = cleaned
	}
	if consumerErr != nil {
		return r, ErrConsumerFailed
	}
	if cleanupErr != nil {
		return r, ErrCleanupPending
	}
	return r, nil
}

// Renew requires the original exact owner to remain active. Provider TTL is
// authoritative; the requested increment is never treated as a grant.
func (m *Manager) Renew(ctx context.Context, o Owner, id string) (*Record, error) {
	r, err := m.owned(ctx, o, id)
	if err != nil {
		return nil, err
	}
	b, err := m.binding(r)
	if err != nil {
		return r, err
	}
	current, active := m.active[r.BindingID]
	if !active || b.Retired || current.Revision != r.BindingRevision {
		return r, ErrStaleOwner
	}
	if r.CleanupRequested || r.State != StateActive || !r.Renewable || !r.ExpiresAt.After(m.now()) {
		return r, ErrConflict
	}
	if err := m.check(ctx, o, r.BindingID); err != nil {
		return r, ErrStaleOwner
	}
	handle, err := m.keys.open(r)
	if err != nil {
		return r, ErrUnavailable
	}
	n := clone(r)
	n.State = StateRenewing
	n.OperationDeadline = m.now().Add(operationTimeout)
	r, err = m.save(ctx, n)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || m.check(ctx, o, r.BindingID) != nil {
		n = clone(r)
		n.State = StateRenewalUnknown
		n.CleanupRequested = true
		n.ReasonCode = "owner_changed_before_renewal"
		if stored, e := m.saveDetached(n); e == nil {
			r = stored
		}
		_, _ = m.cleanupDetached(o, r.ID)
		return r, ErrStaleOwner
	}
	started := m.now()
	g, renewErr := b.Provider.Renew(ctx, handle, b.RenewIncrement)
	// Preserve a concurrently queued cleanup request. No second renewal is
	// attempted even if the provider acknowledgement or SQL write was lost.
	for range 4 {
		actual, e := m.getDetached(o.TenantID, id)
		if e != nil {
			return r, ErrUnavailable
		}
		if actual.State != StateRenewing {
			return actual, ErrCleanupPending
		}
		n = clone(actual)
		if renewErr != nil {
			n.State = StateRenewalUnknown
			n.CleanupRequested = true
			n.ReasonCode = "renewal_outcome_unknown"
		} else {
			n.State = StateActive
			n.GrantedTTLSeconds = int64(g.TTL / time.Second)
			n.ExpiresAt = started.Add(g.TTL)
			n.Renewable = g.Renewable
			if g.TTL < time.Second || g.TTL > b.MaxTTL {
				n.CleanupRequested = true
				n.ReasonCode = "provider_grant_outside_contract"
			}
		}
		if stored, e := m.saveDetached(n); e == nil {
			r = stored
			break
		}
	}
	if r.State == StateRenewing {
		return r, ErrUnavailable
	}
	if r.CleanupRequested {
		cleaned, _ := m.cleanupDetached(o, id)
		if cleaned != nil {
			r = cleaned
		}
		return r, ErrCleanupPending
	}
	return r, nil
}

// Revoke accepts an old attempt's exact lease owner, without granting it access
// to a newer lease. Only sync provider confirmation makes the record revoked.
func (m *Manager) Revoke(ctx context.Context, o Owner, id string) (*Record, error) {
	for range 6 {
		r, err := m.owned(ctx, o, id)
		if err != nil {
			return nil, err
		}
		if r.State == StateRevoked || r.State == StateNotIssued {
			return r, nil
		}
		if !r.CleanupRequested {
			n := clone(r)
			n.CleanupRequested = true
			stored, e := m.save(ctx, n)
			if e != nil {
				continue
			}
			r = stored
		}
		if (r.State == StateIssuing || r.State == StateRenewing || r.State == StateRevoking) && m.now().Before(r.OperationDeadline) {
			return r, ErrCleanupPending
		}
		if r.ProtectedHandle == "" {
			return r, ErrCleanupPending
		}
		b, err := m.binding(r)
		if err != nil {
			return r, ErrUnavailable
		}
		handle, err := m.keys.open(r)
		if err != nil {
			return r, ErrUnavailable
		}
		n := clone(r)
		n.State = StateRevoking
		n.OperationDeadline = m.now().Add(operationTimeout)
		n.ReasonCode = "revocation_requested"
		r, err = m.save(ctx, n)
		if err != nil {
			continue
		}
		revokeErr := b.Provider.Revoke(ctx, handle)
		n = clone(r)
		if revokeErr != nil {
			n.State = StateRevocationUnknown
			n.ReasonCode = "revocation_unconfirmed"
		} else {
			n.State = StateRevoked
			n.ReasonCode = "provider_confirmed_revoked"
			n.Renewable = false
			n.ProtectedHandle = ""
		}
		stored, saveErr := m.saveDetached(n)
		if saveErr != nil {
			return r, ErrCleanupPending
		}
		if revokeErr != nil {
			return stored, ErrCleanupPending
		}
		return stored, nil
	}
	return nil, ErrConflict
}

// Reconcile never repeats issuance or tool execution. It recovers known handles
// by lookup and exact revocation; a lost issuance handle stays explicitly unknown.
func (m *Manager) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrUnavailable
	}
	rows, err := m.store.ListUnresolved(ctx, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	failed := false
	for _, r := range rows {
		if ctx.Err() != nil {
			return count, ErrUnavailable
		}
		if (r.State == StateIssuing || r.State == StateRenewing || r.State == StateRevoking) && m.now().Before(r.OperationDeadline) {
			continue
		}
		if r.State == StateIssuing && r.ProtectedHandle == "" {
			n := clone(r)
			n.State = StateIssuanceUnknown
			n.CleanupRequested = true
			n.ReasonCode = "issuance_acknowledgement_lost"
			if _, err := m.save(ctx, n); err == nil {
				count++
			} else {
				failed = true
			}
			continue
		}
		if r.ProtectedHandle == "" {
			continue
		}
		if r.State == StateActive && !r.CleanupRequested && r.ExpiresAt.After(m.now()) && m.check(ctx, r.Owner, r.BindingID) == nil {
			// Rotate a bounded scan past still-active rows so an older active
			// lease cannot indefinitely hide a newer abandoned lease.
			if _, err := m.save(ctx, clone(r)); err != nil {
				failed = true
			}
			continue
		}
		b, err := m.binding(r)
		if err != nil {
			_, _ = m.save(ctx, clone(r))
			failed = true
			continue
		}
		handle, err := m.keys.open(r)
		if err != nil {
			_, _ = m.save(ctx, clone(r))
			failed = true
			continue
		}
		status, err := b.Provider.Lookup(ctx, handle)
		if err != nil {
			_, _ = m.save(ctx, clone(r))
			failed = true
			continue
		}
		if !status.Found {
			n := clone(r)
			n.State = StateRevoked
			n.CleanupRequested = true
			n.Renewable = false
			n.ProtectedHandle = ""
			n.ReasonCode = "provider_confirmed_absent"
			if _, err := m.save(ctx, n); err == nil {
				count++
			} else {
				failed = true
			}
			continue
		}
		if _, err := m.Revoke(ctx, r.Owner, r.ID); err == nil {
			count++
		} else {
			failed = true
		}
	}
	if failed {
		return count, ErrCleanupPending
	}
	return count, nil
}
