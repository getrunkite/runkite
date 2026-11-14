package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestWorkloadOwnerExpiryNeverBecomesMissingIdentityBypass(t *testing.T) {
	owner := &transport.WorkloadIdentity{URI: "spiffe://fixture.test/worker", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(-time.Second)}
	metadata := map[string]interface{}{AttemptGenerationKey: int64(1), WorkloadOwnerKey: owner}
	ctx := transport.WithWorkloadIdentity(context.Background(), owner)
	if CheckWorkloadOwner(ctx, metadata) == nil || CheckWorkloadOwner(context.Background(), metadata) == nil {
		t.Fatal("expired/missing proof retained owner authority")
	}
	if CheckRunCompletionOwner(ctx, metadata, &RunCompletion{}) == nil {
		t.Fatal("expired owner completed normally")
	}
	if err := CheckRunCompletionOwner(context.Background(), metadata, &RunCompletion{TrustedSystem: true}); err != nil {
		t.Fatal("system recovery lost availability", err)
	}
	if err := BindAttemptWorkload(context.Background(), metadata, 2); err == nil {
		t.Fatal("reclaim removed an owned run's identity")
	}
	if err := BindAttemptWorkload(ctx, metadata, 2); err == nil {
		t.Fatal("expired identity claimed replacement attempt")
	}
	if err := BindAttemptWorkload(WithTrustedAttemptRecovery(ctx), metadata, 2); err != nil {
		t.Fatal("trusted retained-attempt recovery lost availability", err)
	}
	if err := CheckWorkloadOwner(ctx, metadata); err == nil {
		t.Fatal("trusted cleanup extended expired runner authority")
	}
}
