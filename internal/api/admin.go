// Package api: Admin API -- a web dashboard for managing agents, viewing
// runs/threads, debugging, connector status, metrics, and user management.
//
// Scope, stated plainly: this covers the OPERATIONAL half of that list --
// agents, threads, runs (with live debugging via the same SSE stream
// clients use), connector status, cron schedules, and a summary overview.
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
	"net/http"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
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

// AdminOverview is the dashboard's summary view. Counts are derived from
// the same Search* methods as the list views (capped at a generous limit,
// not a real SQL COUNT) -- ponytail: fine for the scale a single admin
// dashboard is used at; a deployment with enough rows for this cap to
// matter would want a real COUNT query added to state.Store instead of
// fetching-then-counting.
type AdminOverview struct {
	TotalAgents       int            `json:"total_agents"`
	TotalThreads      int            `json:"total_threads"`
	ThreadsByStatus   map[string]int `json:"threads_by_status"`
	TotalRuns         int            `json:"total_runs"`
	RunsByStatus      map[string]int `json:"runs_by_status"`
	ConnectorCount    int            `json:"connector_count"`
	CronScheduleCount int            `json:"cron_schedule_count"`
}

const overviewSampleLimit = 1000

// GET /admin-api/overview
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())

	agents, err := s.store.SearchAgents(ctx, &models.AgentSearchRequest{Limit: overviewSampleLimit})
	if err != nil {
		handleStoreError(w, err)
		return
	}

	threads, err := s.store.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: overviewSampleLimit})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	threadsByStatus := map[string]int{}
	for _, t := range threads {
		threadsByStatus[string(t.Status)]++
	}

	runs, err := s.store.SearchRuns(ctx, &models.RunSearchRequest{Limit: overviewSampleLimit})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	runsByStatus := map[string]int{}
	for _, run := range runs {
		runsByStatus[string(run.Status)]++
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
		TotalAgents:       len(agents),
		TotalThreads:      len(threads),
		ThreadsByStatus:   threadsByStatus,
		TotalRuns:         len(runs),
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

// GET /admin-api/agents
func (s *Server) handleAdminListAgents(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	agents, err := s.store.SearchAgents(ctx, &models.AgentSearchRequest{Limit: overviewSampleLimit})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminAgentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, toAdminAgentView(a))
	}
	writeJSON(w, http.StatusOK, views)
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

// GET /admin-api/registry
func (s *Server) handleAdminListRegistryEntries(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	entries, err := s.store.SearchRegistryEntries(ctx, &models.RegistrySearchRequest{Limit: overviewSampleLimit})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminRegistryEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, toAdminRegistryEntryView(e))
	}
	writeJSON(w, http.StatusOK, views)
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

// GET /admin-api/threads
func (s *Server) handleAdminListThreads(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	req := models.ThreadSearchRequest{Limit: overviewSampleLimit}
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
	writeJSON(w, http.StatusOK, views)
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

// GET /admin-api/threads/{threadID}/runs -- same data as the client-facing
// list, but with tenant_id visible (models.Run hides it via json:"-").
func (s *Server) handleAdminListThreadRuns(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	threadID := r.PathValue("threadID")
	runs, err := s.store.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: threadID, Limit: 100})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	views := make([]adminRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toAdminRunView(run))
	}
	writeJSON(w, http.StatusOK, views)
}

// GET /admin-api/runs -- optional ?status=&agent_id=&thread_id= filters.
func (s *Server) handleAdminListRuns(w http.ResponseWriter, r *http.Request) {
	ctx := tenant.SystemContext(r.Context())
	req := models.RunSearchRequest{
		Limit:    overviewSampleLimit,
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
	writeJSON(w, http.StatusOK, views)
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

	dls, err := s.store.ListWebhookDeadLetters(r.Context(), overviewSampleLimit)
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
