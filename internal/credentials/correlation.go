package credentials

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

type correlationKey struct{}
type providerCorrelation struct{ action, lease string }

type reconciliationCheckKey struct{}

// IsReconciliationCheck distinguishes background liveness inspection from an
// attempt to release credentials. Only Manager.Reconcile stamps this context.
func IsReconciliationCheck(ctx context.Context) bool {
	value, _ := ctx.Value(reconciliationCheckKey{}).(bool)
	return value
}

// Provider correlation comes from the durable server-owned lease owner. It is
// not authorization, a raw Vault lease handle or an arbitrary request header.
func withProviderCorrelation(ctx context.Context, o Owner, lease string) context.Context {
	return context.WithValue(ctx, correlationKey{}, providerCorrelation{o.ActionID, lease})
}

func safeCorrelationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func addProviderCorrelation(ctx context.Context, header http.Header) {
	value, ok := ctx.Value(correlationKey{}).(providerCorrelation)
	if ok && safeCorrelationID(value.action) && safeCorrelationID(value.lease) {
		header.Set("X-Runkite-Action-Id", value.action)
		header.Set("X-Runkite-Lease-Ref", value.lease)
	}
}

// Vault request IDs are public UUID correlation facts. Ignore arbitrary provider
// text instead of accepting it as safe evidence because it has an ID-shaped key.
func safeVaultRequestID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return ""
	}
	return value
}
