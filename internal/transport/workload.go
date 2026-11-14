package transport

import (
	"context"
	"time"
)

// WorkloadIdentity is proof derived by the server from a verified client
// certificate. URI identifies the externally issued workload and KeySHA256
// identifies its public key. It is never accepted from request headers or JSON.
type WorkloadIdentity struct {
	URI       string    `json:"uri"`
	KeySHA256 string    `json:"key_sha256"`
	NotAfter  time.Time `json:"not_after"`
}

type workloadIdentityKey struct{}
type trustedQueueKey struct{}

// TrustedQueueContext is only for control-plane recovery and failed-delivery
// cleanup. No wire value can select this capability. Generation still fences it.
func TrustedQueueContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedQueueKey{}, true)
}
func IsTrustedQueueContext(ctx context.Context) bool {
	trusted, _ := ctx.Value(trustedQueueKey{}).(bool)
	return trusted
}

func WithWorkloadIdentity(ctx context.Context, identity *WorkloadIdentity) context.Context {
	if identity == nil {
		return ctx
	}
	copy := *identity
	return context.WithValue(ctx, workloadIdentityKey{}, copy)
}

// WorkloadIdentityFromContext returns a copy of the server's certificate proof.
func WorkloadIdentityFromContext(ctx context.Context) *WorkloadIdentity {
	identity, ok := ctx.Value(workloadIdentityKey{}).(WorkloadIdentity)
	if !ok {
		return nil
	}
	return &identity
}

func (identity *WorkloadIdentity) ValidAt(now time.Time) bool {
	if identity == nil || identity.URI == "" || len(identity.KeySHA256) != 64 || !now.Before(identity.NotAfter) {
		return false
	}
	for _, c := range identity.KeySHA256 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// MatchesWorkload requires the original credential, including its expiry.
// Certificate rotation drains or reclaims existing assignments rather than
// silently extending the authority of an old dispatch.
func MatchesWorkload(owner, actor *WorkloadIdentity, now time.Time) bool {
	return owner.ValidAt(now) && actor.ValidAt(now) && owner.URI == actor.URI && owner.KeySHA256 == actor.KeySHA256 && owner.NotAfter.Equal(actor.NotAfter)
}

// WorkloadIdentityQueue marks queues that capture certificate ownership in the
// same operation that records an in-flight assignment. Custom transports must
// implement the same invariant before enabling strict workload identity.
type WorkloadIdentityQueue interface {
	SupportsWorkloadIdentity() bool
}
