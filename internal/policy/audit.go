package policy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// AuditStore is the narrow write path for policy decisions. Implemented
// by the Postgres state store in Phase 1; other backends omit it.
type AuditStore interface {
	WriteAuditEvent(ctx context.Context, ev *models.AuditEvent) error
}

type storeAuditor struct {
	store AuditStore
}

// NewStoreAuditor wraps a Postgres-backed AuditStore.
func NewStoreAuditor(store AuditStore) Auditor {
	if store == nil {
		return nil
	}
	return &storeAuditor{store: store}
}

func (a *storeAuditor) WritePolicyDecision(ctx context.Context, in PolicyInput, dec PolicyDecision) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	tenantID := in.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	ev := &models.AuditEvent{
		ID:           id,
		TS:           time.Now().UTC(),
		TenantID:     tenantID,
		Actor:        in.Principal,
		Action:       in.Stage,
		ResourceType: "connector",
		ResourceID:   in.Connector,
		Decision:     dec.Effect,
		ReasonCode:   dec.ReasonCode,
		RuleID:       dec.RuleID,
		LatencyMs:    dec.LatencyMs,
		RunID:        in.RunID,
		Generation:   in.Generation,
		AgentID:      in.AgentID,
		Connector:    in.Connector,
		Tool:         in.Tool,
		Attrs: map[string]interface{}{
			"reason": dec.Reason,
		},
	}
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		ev.TraceID = sc.TraceID().String()
	}
	return a.store.WriteAuditEvent(ctx, ev)
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
