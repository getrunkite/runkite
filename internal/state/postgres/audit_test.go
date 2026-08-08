package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Live coverage for the Phase 1 audit write path and migration v2.
// The in-memory policy suite never touches Postgres; this is the
// committed counterpart of the throwaway round-trip used to land v2.
func TestAuditEvents_WriteAndMigrateV2(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres audit tests")
	}

	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bk := migrate.NewPgx(s.pool)
	cur, err := bk.Current(ctx)
	if err != nil || cur != 3 {
		t.Fatalf("version after Init = %d, %v; want 3", cur, err)
	}
	if !tableExists(t, ctx, s, "audit_events") {
		t.Fatal("audit_events missing after Init")
	}

	// Downgrade v3 then v2: drop policy_grants, then audit_events; re-Init restores both.
	if err := s.Downgrade(ctx); err != nil {
		t.Fatalf("Downgrade v3→v2: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 2 {
		t.Fatalf("version after Downgrade = %d, want 2", cur)
	}
	if tableExists(t, ctx, s, "policy_grants") {
		t.Fatal("policy_grants still present after v3 Down")
	}
	if err := s.Downgrade(ctx); err != nil {
		t.Fatalf("Downgrade v2→v1: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 1 {
		t.Fatalf("version after Downgrade = %d, want 1", cur)
	}
	if tableExists(t, ctx, s, "audit_events") {
		t.Fatal("audit_events still present after v2 Down")
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-Init after Downgrade: %v", err)
	}
	cur, _ = bk.Current(ctx)
	if cur != 3 {
		t.Fatalf("version after re-Init = %d, want 3", cur)
	}
	if !tableExists(t, ctx, s, "audit_events") || !tableExists(t, ctx, s, "policy_grants") {
		t.Fatal("audit_events/policy_grants missing after re-Init")
	}

	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	ev := &models.AuditEvent{
		ID:           "audit-test-" + ts.Format("20060102150405"),
		TS:           ts,
		TenantID:     "acme",
		Actor:        "runner-1",
		Action:       "tool.call",
		ResourceType: "connector",
		ResourceID:   "salesforce",
		Decision:     "deny",
		ReasonCode:   "policy_no_grant",
		RuleID:       "",
		LatencyMs:    3,
		RunID:        "run-abc",
		Generation:   2,
		AgentID:      "sales-assistant",
		Connector:    "salesforce",
		Tool:         "updateRecord",
		Attrs:        map[string]interface{}{"reason": "no matching grant"},
		TraceID:      "trace-xyz",
	}
	if err := s.WriteAuditEvent(ctx, ev); err != nil {
		t.Fatalf("WriteAuditEvent: %v", err)
	}

	var (
		gotTenant, gotActor, gotAction, gotResType, gotResID string
		gotDecision, gotReason, gotRuleID, gotRunID          string
		gotAgent, gotConnector, gotTool, gotTrace            string
		gotLatency                                           int
		gotGen                                               int64
		gotTS                                                time.Time
		attrsRaw                                             []byte
	)
	err = s.pool.QueryRow(ctx, `
		SELECT ts, tenant_id, actor, action, resource_type, resource_id,
		       decision, reason_code, rule_id, latency_ms,
		       run_id, generation, agent_id, connector, tool, attrs, trace_id
		FROM audit_events WHERE id = $1
	`, ev.ID).Scan(
		&gotTS, &gotTenant, &gotActor, &gotAction, &gotResType, &gotResID,
		&gotDecision, &gotReason, &gotRuleID, &gotLatency,
		&gotRunID, &gotGen, &gotAgent, &gotConnector, &gotTool, &attrsRaw, &gotTrace,
	)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("ts = %v, want %v", gotTS, ts)
	}
	if gotTenant != ev.TenantID || gotActor != ev.Actor || gotAction != ev.Action ||
		gotResType != ev.ResourceType || gotResID != ev.ResourceID ||
		gotDecision != ev.Decision || gotReason != ev.ReasonCode ||
		gotRuleID != ev.RuleID || gotLatency != ev.LatencyMs ||
		gotRunID != ev.RunID || gotGen != ev.Generation ||
		gotAgent != ev.AgentID || gotConnector != ev.Connector ||
		gotTool != ev.Tool || gotTrace != ev.TraceID {
		t.Fatalf("column mismatch: got tenant=%q actor=%q action=%q res=%q/%q decision=%q reason=%q rule=%q latency=%d run=%q gen=%d agent=%q connector=%q tool=%q trace=%q",
			gotTenant, gotActor, gotAction, gotResType, gotResID, gotDecision, gotReason, gotRuleID, gotLatency, gotRunID, gotGen, gotAgent, gotConnector, gotTool, gotTrace)
	}
	var attrs map[string]interface{}
	if err := json.Unmarshal(attrsRaw, &attrs); err != nil {
		t.Fatalf("attrs json: %v", err)
	}
	if attrs["reason"] != "no matching grant" {
		t.Fatalf("attrs.reason = %v, want %q", attrs["reason"], "no matching grant")
	}

	err = s.WriteAuditEvent(ctx, ev)
	if err == nil {
		t.Fatal("duplicate WriteAuditEvent: want primary-key error, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate WriteAuditEvent: want unique_violation (23505), got %v", err)
	}

	// Leave the shared test DB at the current schema for sibling tests.
	if _, err := s.pool.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, ev.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestSearchAuditEvents_FiltersAndCursor(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping Postgres audit tests")
	}

	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM audit_events WHERE id LIKE 'search-audit-%'`); err != nil {
		t.Fatalf("cleanup prefix: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM audit_events WHERE id LIKE 'search-audit-%'`)
	})

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	write := func(id, tenant, decision, runID string, offset time.Duration) {
		t.Helper()
		ev := &models.AuditEvent{
			ID: id, TS: base.Add(offset), TenantID: tenant, Action: "tool.call",
			Decision: decision, RunID: runID, AgentID: "sales", Connector: "salesforce",
			Tool: "updateRecord", Attrs: map[string]interface{}{"reason": "test"},
		}
		if err := s.WriteAuditEvent(ctx, ev); err != nil {
			t.Fatalf("WriteAuditEvent(%s): %v", id, err)
		}
	}
	write("search-audit-a1", "acme", "deny", "run-a", 0)
	write("search-audit-a2", "acme", "allow", "run-a", time.Minute)
	write("search-audit-b1", "beta", "deny", "run-b", 2*time.Minute)
	write("search-audit-a3", "acme", "deny", "run-c", 3*time.Minute)

	sys := tenant.SystemContext(ctx)
	denied, err := s.SearchAuditEvents(sys, &models.AuditSearchRequest{
		TenantID: "acme", Decision: "deny", Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchAuditEvents: %v", err)
	}
	if len(denied) != 2 {
		t.Fatalf("acme deny count = %d, want 2", len(denied))
	}
	if denied[0].ID != "search-audit-a3" || denied[1].ID != "search-audit-a1" {
		t.Fatalf("order = [%s, %s], want [a3, a1] newest-first", denied[0].ID, denied[1].ID)
	}

	since := base.Add(90 * time.Second)
	until := base.Add(4 * time.Minute)
	window, err := s.SearchAuditEvents(sys, &models.AuditSearchRequest{
		TenantID: "acme", Decision: "deny", Since: &since, Until: &until, Limit: 50,
	})
	if err != nil {
		t.Fatalf("window search: %v", err)
	}
	if len(window) != 1 || window[0].ID != "search-audit-a3" {
		t.Fatalf("since/until window = %+v, want [a3]", idsOf(window))
	}

	page1, err := s.SearchAuditEvents(sys, &models.AuditSearchRequest{
		TenantID: "acme", Limit: 2,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	cursor := pagecursor.EncodeTime(page1[1].TS, page1[1].ID)
	page2, err := s.SearchAuditEvents(sys, &models.AuditSearchRequest{
		TenantID: "acme", Limit: 2, Cursor: cursor,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != "search-audit-a1" {
		t.Fatalf("page2 = %+v, want [a1]", idsOf(page2))
	}

	scoped := tenant.WithContext(ctx, "beta")
	onlyBeta, err := s.SearchAuditEvents(scoped, &models.AuditSearchRequest{Limit: 50})
	if err != nil {
		t.Fatalf("tenant-scoped: %v", err)
	}
	if len(onlyBeta) != 1 || onlyBeta[0].ID != "search-audit-b1" {
		t.Fatalf("beta scope = %+v, want [b1]", idsOf(onlyBeta))
	}
}

func idsOf(events []*models.AuditEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func tableExists(t *testing.T, ctx context.Context, s *Store, name string) bool {
	t.Helper()
	ok, err := migrate.PgxTableExists(ctx, s.pool, name)
	if err != nil {
		t.Fatalf("tableExists(%s): %v", name, err)
	}
	return ok
}
