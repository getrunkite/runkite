// Package credentials coordinates externally issued, short-lived database
// credentials. It never implements a vault or persists credential values.
package credentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrUnavailable     = errors.New("credential lifecycle unavailable")
	ErrConflict        = errors.New("credential lifecycle conflict")
	ErrNotFound        = errors.New("credential lease not found")
	ErrStaleOwner      = errors.New("credential lease owner is no longer active")
	ErrAlreadyIssued   = errors.New("credential issuance or use already attempted")
	ErrIssuanceUnknown = errors.New("credential issuance outcome unknown; do not retry")
	ErrCleanupPending  = errors.New("credential cleanup is not provider-confirmed")
	ErrConsumerFailed  = errors.New("credential consumer failed; inspect action evidence")
)

// Owner must come from trusted assignment and durable action context, never
// agent-provided arguments. RunIncarnationID distinguishes reused run IDs.
type Owner struct {
	TenantID         string `json:"tenant_id"`
	RunID            string `json:"run_id"`
	RunIncarnationID string `json:"run_incarnation_id"`
	Generation       int64  `json:"generation"`
	ActionID         string `json:"action_id"`
	ConsumerID       string `json:"consumer_id"`
}

func (o Owner) valid() bool {
	return o.TenantID != "" && o.RunID != "" && o.RunIncarnationID != "" && o.Generation > 0 && o.ActionID != "" && o.ConsumerID != ""
}

type State string

const (
	StateIssuing           State = "issuance_intent"
	StateActive            State = "active"
	StateIssuanceUnknown   State = "issuance_unknown"
	StateNotIssued         State = "not_issued"
	StateRenewing          State = "renewal_intent"
	StateRenewalUnknown    State = "renewal_unknown"
	StateRevoking          State = "revocation_intent"
	StateRevocationUnknown State = "revocation_unknown"
	StateRevoked           State = "revoked"
)

// Record is safe for metadata views. ProtectedHandle is internal encrypted
// cleanup material and must never appear in JSON, logs or public DTOs.
type Record struct {
	ID                string     `json:"lease_ref"`
	Owner             Owner      `json:"owner"`
	BindingID         string     `json:"binding_id"`
	BindingRevision   string     `json:"binding_revision"`
	State             State      `json:"state"`
	Version           int64      `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	OperationDeadline time.Time  `json:"operation_deadline"`
	ExpiresAt         time.Time  `json:"expires_at"`
	GrantedTTLSeconds int64      `json:"granted_ttl_seconds"`
	Renewable         bool       `json:"renewable"`
	UseStartedAt      *time.Time `json:"use_started_at,omitempty"`
	CleanupRequested  bool       `json:"cleanup_requested"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	ProtectedHandle   string     `json:"-"`
}

func (r Record) String() string   { b, _ := json.Marshal(r); return string(b) }
func (r Record) GoString() string { return r.String() }

// Store mutations are compare-and-swap. Create is unique on the complete owner
// and binding ID even if configuration changes; replay must return created=false.
// SQL tables have no run FK so old leases can still be cleaned after retention.
type Store interface {
	Create(context.Context, *Record) (*Record, bool, error)
	Get(context.Context, string, string) (*Record, error)
	CAS(context.Context, *Record, int64) (*Record, error)
	ListUnresolved(context.Context, int) ([]*Record, error)
	Search(context.Context, string, int) ([]*Record, error)
}

// ActiveChecker rechecks tenant, exact run incarnation/generation, action
// authority and registered consumer binding. Revoke deliberately does not use
// it: a superseded attempt's exact old lease still requires cleanup.
type ActiveChecker func(context.Context, Owner, string) error

// UserPassword is deliberately not serializable or printable. Only a trusted
// executor callback may access these ephemeral credential values.
type UserPassword struct{ username, password string }

// NewUserPassword is for trusted provider implementations, not request parsing.
func NewUserPassword(username, password string) (UserPassword, error) {
	if username == "" || password == "" || len(username) > 4096 || len(password) > 16384 || strings.ContainsRune(username, 0) || strings.ContainsRune(password, 0) {
		return UserPassword{}, ErrUnavailable
	}
	return UserPassword{username, password}, nil
}

func (UserPassword) String() string               { return "[redacted database credentials]" }
func (u UserPassword) GoString() string           { return u.String() }
func (UserPassword) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }
func (u UserPassword) Values() (string, string)   { return u.username, u.password }

type Grant struct {
	Handle     string
	TTL        time.Duration
	Renewable  bool
	Credential UserPassword
}

func (Grant) String() string               { return "[redacted provider grant]" }
func (g Grant) GoString() string           { return g.String() }
func (Grant) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

type LeaseStatus struct {
	Found     bool
	TTL       time.Duration
	Renewable bool
}

// Provider does not retry issuance. A timed-out GET can have issued a real
// credential, so callers must retain an unknown record instead of issuing again.
type Provider interface {
	Issue(context.Context) (Grant, error)
	Renew(context.Context, string, time.Duration) (Grant, error)
	Revoke(context.Context, string) error
	Lookup(context.Context, string) (LeaseStatus, error)
}

// Binding is operator-registered. Provider and revision are frozen for this
// binding's lifetime. Keep retired revisions registered until cleanup completes.
type Binding struct {
	ID             string
	Revision       string
	ConsumerID     string
	MaxTTL         time.Duration
	RenewIncrement time.Duration
	Provider       Provider
	Retired        bool
}

func ownerKey(o Owner, bindingID string) string {
	b, _ := json.Marshal(struct {
		Owner   Owner
		Binding string
	}{o, bindingID})
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func clone(r *Record) *Record {
	if r == nil {
		return nil
	}
	n := *r
	if r.UseStartedAt != nil {
		t := *r.UseStartedAt
		n.UseStartedAt = &t
	}
	return &n
}
