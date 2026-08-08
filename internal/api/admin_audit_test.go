package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdminListAuditEvents_NonSQLStoreReturns501(t *testing.T) {
	// nil store does not implement SearchAuditEvents (same as Mongo).
	srv := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/audit-events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 501 on non-SQL store, got %d: %s", resp.StatusCode, body)
	}
}

func TestAdminListAuditEvents_FiltersAndCursor(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping audit Admin API tests")
	}

	ctx := context.Background()
	store, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	prefix := "admin-audit-" + time.Now().UTC().Format("20060102150405.000")
	ids := []string{prefix + "-a1", prefix + "-a2", prefix + "-b1", prefix + "-a3"}
	t.Cleanup(func() { deleteAuditIDs(t, dsn, ids) })

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	for _, row := range []struct {
		id, tenant, decision string
		ts                   time.Time
	}{
		{ids[0], "acme", "deny", base},
		{ids[1], "acme", "allow", base.Add(time.Minute)},
		{ids[2], "beta", "deny", base.Add(2 * time.Minute)},
		{ids[3], "acme", "deny", base.Add(3 * time.Minute)},
	} {
		if err := store.WriteAuditEvent(ctx, &models.AuditEvent{
			ID: row.id, TS: row.ts, TenantID: row.tenant, Action: "tool.call",
			Decision: row.decision, RunID: "run-" + row.id, AgentID: "sales",
			Connector: "salesforce", Tool: "updateRecord",
		}); err != nil {
			t.Fatalf("WriteAuditEvent(%s): %v", row.id, err)
		}
	}

	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	cancelBus := inprocess.NewCancelBus()
	apiServer := api.NewServer(store, queue, broker, cancelBus)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin-api/audit-events?tenant_id=acme&decision=deny&limit=50")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var denied []models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&denied); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(denied) != 2 {
		t.Fatalf("acme deny count = %d, want 2", len(denied))
	}
	if denied[0].ID != ids[3] || denied[1].ID != ids[0] {
		t.Fatalf("order = [%s, %s], want [%s, %s]", denied[0].ID, denied[1].ID, ids[3], ids[0])
	}

	pageResp, err := http.Get(srv.URL + "/admin-api/audit-events?tenant_id=acme&limit=2")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	defer pageResp.Body.Close()
	cursor := pageResp.Header.Get(pagecursor.HeaderNextCursor)
	if cursor == "" {
		t.Fatal("expected X-Next-Cursor on full page")
	}
	var page1 []models.AuditEvent
	if err := json.NewDecoder(pageResp.Body).Decode(&page1); err != nil {
		t.Fatalf("page1 decode: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	page2URL := srv.URL + "/admin-api/audit-events?tenant_id=acme&limit=2&cursor=" + url.QueryEscape(cursor)
	page2Resp, err := http.Get(page2URL)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	defer page2Resp.Body.Close()
	var page2 []models.AuditEvent
	if err := json.NewDecoder(page2Resp.Body).Decode(&page2); err != nil {
		t.Fatalf("page2 decode: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != ids[0] {
		t.Fatalf("page2 ids = %v, want [%s]", auditIDs(page2), ids[0])
	}

	bad, err := http.Get(srv.URL + "/admin-api/audit-events?since=not-a-date")
	if err != nil {
		t.Fatalf("bad since: %v", err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since: want 400, got %d", bad.StatusCode)
	}
}

func deleteAuditIDs(t *testing.T, dsn string, ids []string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Logf("cleanup connect: %v", err)
		return
	}
	defer pool.Close()
	for _, id := range ids {
		if _, err := pool.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, id); err != nil {
			t.Logf("cleanup %s: %v", id, err)
		}
	}
}

func auditIDs(events []models.AuditEvent) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].ID
	}
	return out
}
