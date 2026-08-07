// Package api: Admin API -- a web dashboard for managing agents, viewing
// runs/threads, debugging, connector status, metrics, and user management.
//
// Scope, stated plainly: this covers the OPERATIONAL half of that list --
// agents, threads, runs (with live debugging via the same SSE stream
// clients use), connector status, cron schedules, policy audit search
// (Postgres Supported), and a summary overview.
// "User management" is explicitly NOT built here: there is no persisted
// user/API-key table today (auth.APIKeyEntry is static langgraph.json
// config, not a DB-backed model) -- building real user CRUD means
// inventing that persistence subsystem first, which is its own separate
// piece of work, not a UI feature to bolt on. Every /admin-api/* route sees
// across every tenant (auth.Middleware enforces "admin" permission
// specifically for this path prefix; see internal/auth/auth.go) -- an
// operator managing the whole deployment, not one tenant's own view.
//
// Most routes are thin system-context wrappers around the exact same
// handlers the client-facing API already uses (see withSystemContext) --
// reusing them instead of duplicating list/search/get logic. Agents and
// threads get dedicated admin views instead, specifically to surface
// tenant_id (models.Agent/Thread/Run.TenantID is deliberately hidden from
// the public Agent Protocol response shape via `json:"-"`, but an admin
// managing a multi-tenant deployment needs to see which tenant owns each
// row).
//
// Deliberately namespaced /admin-api/* rather than /admin/*: the built
// React dashboard's static assets are served at /admin/* (see
// cmd/serve.go's embed.FS wiring) -- a JSON API and a static SPA sharing
// one path prefix would collide (e.g. GET /admin/agents meaning both "the
// agents API" and "a static file named agents").
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/tenant"
)

// withSystemContext wraps a handler that already reads r.Context() (every
// existing handler in this package does) so it operates across every
// tenant instead of whichever tenant the caller's own auth identity
// belongs to. Only ever mounted under /admin-api/*, never on the client-facing
// routes those same handler functions are also registered under.
func withSystemContext(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(tenant.SystemContext(r.Context())))
	}
}

// --------------------------------------------------------------------------
// Overview
// --------------------------------------------------------------------------

// AdminOverview is the dashboard's summary view. Totals and by-status
// breakdowns come from store COUNT / GROUP BY aggregates (not
// fetch-then-len of a Search* page), so a soak that creates tens of
// thousands of rows still reports honest numbers.
type AdminOverview struct {
	TotalAgents       int            `json:"total_agents"`
	TotalThreads      int            `json:"total_threads"`
	ThreadsByStatus   map[string]int `json:"threads_by_status"`
	TotalRuns         int            `json:"total_runs"`
	RunsByStatus      map[string]int `json:"runs_by_status"`
	ConnectorCount    int            `json:"connector_count"`
	CronScheduleCount int            `json:"cron_schedule_count"`
}

// Admin list endpoints (Agents/Threads/Runs/Registry) take ?limit=&offset=
// and optionally ?cursor= (keyset). Response is a bare JSON array; has-more
// when len == limit. When another page exists, X-Next-Cursor carries the
// opaque resume token (see internal/pagecursor).
const (
	adminListDefaultLimit = 50
	adminListMaxLimit     = 200
)

type adminPaging struct {
	Limit  int
	Offset int
	Cursor string
}

// adminListPaging reads ?limit=&offset=&cursor= for Admin list handlers.
// cursor and offset are mutually exclusive (either query present → 400).
func adminListPaging(r *http.Request) (adminPaging, error) {
	p := adminPaging{Limit: adminListDefaultLimit}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if p.Limit > adminListMaxLimit {
		p.Limit = adminListMaxLimit
	}
	p.Cursor = r.URL.Query().Get("cursor")
	_, hasOffset := r.URL.Query()["offset"]
	if p.Cursor != "" && hasOffset {
		return p, fmt.Errorf("cursor and offset are mutually exclusive")
	}
	if hasOffset {
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				p.Offset = n
			}
		}
	}
	return p, nil
}

func writeAdminListJSON(w http.ResponseWriter, status int, v interface{}, nextCursor string) {
	w.Header().Set("Content-Type", "application/json")
	if nextCursor != "" {
		w.Header().Set(pagecursor.HeaderNextCursor, nextCursor)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func sumStatusCounts(byStatus map[string]int) int {
	total := 0
	for _, n := range byStatus {
		total += n
	}
	return total
}

// GET /admin-api/overview
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())

	totalAgents, err := s.store.CountAgents(ctx)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	threadsByStatus, err := s.store.CountThreadsByStatus(ctx)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if threadsByStatus == nil {
		threadsByStatus = map[string]int{}
	}

	runsByStatus, err := s.store.CountRunsByStatus(ctx)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if runsByStatus == nil {
		runsByStatus = map[string]int{}
	}

	connectorCount := 0
	if s.connectors != nil {
		connectorCount = len(s.connectors.List())
	}

	cronSchedules, err := s.store.ListCronSchedules(ctx)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, AdminOverview{
		TotalAgents:       totalAgents,
		TotalThreads:      sumStatusCounts(threadsByStatus),
		ThreadsByStatus:   threadsByStatus,
		TotalRuns:         sumStatusCounts(runsByStatus),
		RunsByStatus:      runsByStatus,
		ConnectorCount:    connectorCount,
		CronScheduleCount: len(cronSchedules),
	})
}

// --------------------------------------------------------------------------
// Agents (tenant_id visible)
// --------------------------------------------------------------------------

type adminAgentView struct {
	*models.Agent
	TenantID string `json:"tenant_id"`
}

func toAdminAgentView(a *models.Agent) adminAgentView {
	return adminAgentView{Agent: a, TenantID: a.TenantID}
}

// GET /admin-api/agents?limit=&offset=&cursor=
func (s *Server) handleAdminListAgents(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeKey(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	agents, err := s.store.SearchAgents(ctx, &models.AgentSearchRequest{
		Limit: paging.Limit, Offset: paging.Offset, Cursor: paging.Cursor,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminAgentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, toAdminAgentView(a))
	}
	next := ""
	if len(agents) == paging.Limit && len(agents) > 0 {
		last := agents[len(agents)-1]
		next = pagecursor.EncodeKey(last.Name, last.AgentID)
	}
	writeAdminListJSON(w, http.StatusOK, views, next)
}

// adminScopedAgentContext mirrors adminScopedRegistryContext: agent_id is
// only unique within a tenant. System-context GetAgent under a cross-tenant
// ID collision returns an arbitrary match. ?tenant_id= scopes the lookup.
func adminScopedAgentContext(r *http.Request) context.Context {
	if tid := r.URL.Query().Get("tenant_id"); tid != "" {
		return tenant.WithContext(r.Context(), tid)
	}
	return tenant.SystemContext(r.Context())
}

// GET /admin-api/agents/{agentID}[?tenant_id=]
func (s *Server) handleAdminGetAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.GetAgent(adminScopedAgentContext(r), r.PathValue("agentID"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminAgentView(agent))
}

// --------------------------------------------------------------------------
// Registry (tenant_id visible) -- the agent marketplace / registry
// --------------------------------------------------------------------------

type adminRegistryEntryView struct {
	*models.RegistryEntry
	TenantID string `json:"tenant_id"`
}

func toAdminRegistryEntryView(e *models.RegistryEntry) adminRegistryEntryView {
	return adminRegistryEntryView{RegistryEntry: e, TenantID: e.TenantID}
}

// GET /admin-api/registry?limit=&offset=&cursor=
func (s *Server) handleAdminListRegistryEntries(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeKey(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	entries, err := s.store.SearchRegistryEntries(ctx, &models.RegistrySearchRequest{
		Limit: paging.Limit, Offset: paging.Offset, Cursor: paging.Cursor,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminRegistryEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, toAdminRegistryEntryView(e))
	}
	next := ""
	if len(entries) == paging.Limit && len(entries) > 0 {
		last := entries[len(entries)-1]
		next = pagecursor.EncodeKey(last.Name, last.TenantID)
	}
	writeAdminListJSON(w, http.StatusOK, views, next)
}

// adminScopedRegistryContext resolves the context a registry admin
// lookup should run under. name alone is not a unique key across
// tenants (registry_entries' real primary key is (tenant_id, name)) --
// system context with no tenant filter at all would make GetRegistryEntry
// return an arbitrary match and ListRegistryEntryVersions genuinely
// MERGE two different tenants' version histories into one list under a
// cross-tenant name collision. An explicit ?tenant_id= query param
// disambiguates by scoping to that one tenant instead of system
// context; omitting it keeps the old (ambiguous-under-collision)
// behavior for a single-tenant deployment where no collision can occur.
func adminScopedRegistryContext(r *http.Request) context.Context {
	if tid := r.URL.Query().Get("tenant_id"); tid != "" {
		return tenant.WithContext(r.Context(), tid)
	}
	return tenant.SystemContext(r.Context())
}

// GET /admin-api/registry/{name}[?tenant_id=]
func (s *Server) handleAdminGetRegistryEntry(w http.ResponseWriter, r *http.Request) {
	entry, err := s.store.GetRegistryEntry(adminScopedRegistryContext(r), r.PathValue("name"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminRegistryEntryView(entry))
}

type adminRegistryEntryVersionView struct {
	*models.RegistryEntryVersion
	TenantID string `json:"tenant_id"`
}

// GET /admin-api/registry/{name}/versions[?tenant_id=]
func (s *Server) handleAdminListRegistryEntryVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.store.ListRegistryEntryVersions(adminScopedRegistryContext(r), r.PathValue("name"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	// tenant_id exposed here (unlike the client-facing response shape,
	// where models.RegistryEntryVersion.TenantID is json:"-") so an
	// operator can actually tell two same-named entries' histories
	// apart when ?tenant_id= wasn't specified and they came back merged.
	views := make([]adminRegistryEntryVersionView, 0, len(versions))
	for _, v := range versions {
		views = append(views, adminRegistryEntryVersionView{RegistryEntryVersion: v, TenantID: v.TenantID})
	}
	writeJSON(w, http.StatusOK, views)
}

// --------------------------------------------------------------------------
// Threads (tenant_id visible)
// --------------------------------------------------------------------------

type adminThreadView struct {
	*models.Thread
	TenantID string `json:"tenant_id"`
}

func toAdminThreadView(t *models.Thread) adminThreadView {
	return adminThreadView{Thread: t, TenantID: t.TenantID}
}

// GET /admin-api/threads?limit=&offset=&cursor=&status=
func (s *Server) handleAdminListThreads(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	req := models.ThreadSearchRequest{Limit: paging.Limit, Offset: paging.Offset, Cursor: paging.Cursor}
	if status := r.URL.Query().Get("status"); status != "" {
		st := models.ThreadStatus(status)
		req.Status = &st
	}
	threads, err := s.store.SearchThreads(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminThreadView, 0, len(threads))
	for _, t := range threads {
		views = append(views, toAdminThreadView(t))
	}
	next := ""
	if len(threads) == paging.Limit && len(threads) > 0 {
		last := threads[len(threads)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.ThreadID)
	}
	writeAdminListJSON(w, http.StatusOK, views, next)
}

// GET /admin-api/threads/{threadID}
func (s *Server) handleAdminGetThread(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	thread, err := s.store.GetThread(ctx, r.PathValue("threadID"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminThreadView(thread))
}

// --------------------------------------------------------------------------
// Runs (tenant_id visible)
// --------------------------------------------------------------------------

type adminRunView struct {
	*models.Run
	TenantID string `json:"tenant_id"`
}

func toAdminRunView(run *models.Run) adminRunView {
	return adminRunView{Run: run, TenantID: run.TenantID}
}

// GET /admin-api/threads/{threadID}/runs?limit=&offset=&cursor= -- same data as the
// client-facing list, but with tenant_id visible (models.Run hides it via
// json:"-").
func (s *Server) handleAdminListThreadRuns(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	threadID := r.PathValue("threadID")
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	runs, err := s.store.SearchRuns(ctx, &models.RunSearchRequest{
		ThreadID: threadID, Limit: paging.Limit, Offset: paging.Offset, Cursor: paging.Cursor,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toAdminRunView(run))
	}
	next := ""
	if len(runs) == paging.Limit && len(runs) > 0 {
		last := runs[len(runs)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.RunID)
	}
	writeAdminListJSON(w, http.StatusOK, views, next)
}

// GET /admin-api/runs?limit=&offset=&cursor= -- optional ?status=&agent_id=&thread_id= filters.
func (s *Server) handleAdminListRuns(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	req := models.RunSearchRequest{
		Limit:    paging.Limit,
		Offset:   paging.Offset,
		Cursor:   paging.Cursor,
		AgentID:  r.URL.Query().Get("agent_id"),
		ThreadID: r.URL.Query().Get("thread_id"),
	}
	if status := r.URL.Query().Get("status"); status != "" {
		st := models.RunStatus(status)
		req.Status = &st
	}
	runs, err := s.store.SearchRuns(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toAdminRunView(run))
	}
	next := ""
	if len(runs) == paging.Limit && len(runs) > 0 {
		last := runs[len(runs)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.RunID)
	}
	writeAdminListJSON(w, http.StatusOK, views, next)
}

// GET /admin-api/runs/{runID}
func (s *Server) handleAdminGetRun(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	run, err := s.store.GetRun(ctx, r.PathValue("runID"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminRunView(run))
}

// auditEventStore is the Postgres-only search surface for policy
// decisions. Compatible backends omit it; the Admin handler returns 501.
type auditEventStore interface {
	SearchAuditEvents(ctx context.Context, req *models.AuditSearchRequest) ([]*models.AuditEvent, error)
}

// GET /admin-api/audit-events?limit=&offset=&cursor=
// Optional filters: tenant_id, decision, action, run_id, agent_id,
// connector, tool, since, until (RFC3339; since inclusive, until exclusive).
func (s *Server) handleAdminListAuditEvents(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(auditEventStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "audit search requires Postgres state backend (Supported profile)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	q := r.URL.Query()
	req := models.AuditSearchRequest{
		Limit:     paging.Limit,
		Offset:    paging.Offset,
		Cursor:    paging.Cursor,
		TenantID:  q.Get("tenant_id"),
		Decision:  q.Get("decision"),
		Action:    q.Get("action"),
		RunID:     q.Get("run_id"),
		AgentID:   q.Get("agent_id"),
		Connector: q.Get("connector"),
		Tool:      q.Get("tool"),
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		req.Since = &t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "until must be RFC3339")
			return
		}
		req.Until = &t
	}
	events, err := store.SearchAuditEvents(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if events == nil {
		events = []*models.AuditEvent{}
	}
	next := ""
	if len(events) == paging.Limit && len(events) > 0 {
		last := events[len(events)-1]
		next = pagecursor.EncodeTime(last.TS, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, events, next)
}

// GET /admin-api/runs/{runID}/stream -- live/replayed event debugging, same
// SSE mechanics as the client-facing stream (see streamExistingRun in
// runs.go); reused directly rather than duplicated, wrapped so an admin
// can watch any tenant's run.
func (s *Server) handleAdminStreamRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	r = r.WithContext(tenant.SystemContext(r.Context()))
	s.streamExistingRun(w, r, runID)
}

// --------------------------------------------------------------------------
// Webhook dead-letter redelivery
// --------------------------------------------------------------------------

type redeliverWebhookResponse struct {
	Delivered  bool   `json:"delivered"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

const redeliverTimeout = 10 * time.Second

// POST /admin-api/webhooks/dead-letters/{id}/redeliver -- genuinely new
// (the client-facing API has no equivalent to reuse, unlike cancel/delete
// above). Re-POSTs the dead letter's stored payload to its stored URL.
//
// Known limitation: WebhookDeadLetter doesn't persist the signing secret
// (only URL/payload/etc -- see models.WebhookDeadLetter), so a redelivery
// is sent unsigned. A receiver that enforces X-Runkite-Signature will
// reject it; there's no way around that without also persisting secrets
// in the dead-letter record, which has its own exposure trade-offs. Also
// doesn't remove the dead letter from the list on success -- it stays as
// an audit record; the response's "delivered" field is the UI's signal
// to show success, not a mutation of stored state.
func (s *Server) handleRedeliverWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// ListWebhookDeadLetters has no Get-by-ID; scan a generous page.
	dls, err := s.store.ListWebhookDeadLetters(r.Context(), 1000)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	var target *models.WebhookDeadLetter
	for _, dl := range dls {
		if dl.ID == id {
			target = dl
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "dead letter not found")
		return
	}

	client := &http.Client{Timeout: redeliverTimeout}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.URL, bytes.NewReader(target.Payload))
	if err != nil {
		writeJSON(w, http.StatusOK, redeliverWebhookResponse{Delivered: false, Error: err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, redeliverWebhookResponse{Delivered: false, Error: err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusOK, redeliverWebhookResponse{
			Delivered:  false,
			StatusCode: resp.StatusCode,
			Error:      "endpoint returned status " + resp.Status,
		})
		return
	}
	writeJSON(w, http.StatusOK, redeliverWebhookResponse{Delivered: true, StatusCode: resp.StatusCode})
}
