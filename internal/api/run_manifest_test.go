package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestBuildRunManifestDefaults(t *testing.T) {
	captured := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	m := buildRunManifest(runManifestInput{
		capturedAt: captured,
		tenantID:   "t1",
		agentID:    "agent-a",
	})

	if m.SchemaVersion != runManifestSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", m.SchemaVersion, runManifestSchemaVersion)
	}
	if !m.CapturedAt.Equal(captured) {
		t.Fatalf("captured_at=%v, want %v", m.CapturedAt, captured)
	}
	if m.TenantID != "t1" || m.AgentID != "agent-a" {
		t.Fatalf("tenant/agent: got %q / %q", m.TenantID, m.AgentID)
	}
	if m.RunnerKind != "python-langgraph" {
		t.Fatalf("runner_kind=%q, want python-langgraph default", m.RunnerKind)
	}
	if m.AllowedTools != nil {
		t.Fatalf("allowed_tools should be nil when unset, got %#v", m.AllowedTools)
	}
	if m.Principal != nil {
		t.Fatalf("principal should be nil without user, got %#v", m.Principal)
	}
}

func TestBuildRunManifestFromAgentMetadata(t *testing.T) {
	agent := &models.Agent{
		AgentID: "sales-bot",
		Version: 3,
		Metadata: map[string]interface{}{
			"runner_kind":       "typescript-langgraphjs",
			"connector_needs":   []string{"salesforce", "slack"},
			"allowed_tools":     []interface{}{"search"},
			"allowed_tools_set": true,
		},
	}
	parent := "parent-run-id"
	m := buildRunManifest(runManifestInput{
		capturedAt:       time.Now(),
		tenantID:         "acme",
		agentID:          "sales-bot",
		requestedAlias:   "sales",
		agent:            agent,
		policyFailClosed: true,
		user: &transport.UserContext{
			Identity:    "alice",
			Permissions: []string{"runs:create"},
		},
		parentRunID: &parent,
		depth:       2,
	})

	if m.RequestedAlias != "sales" || m.AgentVersion != 3 {
		t.Fatalf("alias/version: got %q / %d", m.RequestedAlias, m.AgentVersion)
	}
	if m.RunnerKind != "typescript-langgraphjs" {
		t.Fatalf("runner_kind=%q", m.RunnerKind)
	}
	if !reflect.DeepEqual(m.ConnectorNeeds, []string{"salesforce", "slack"}) {
		t.Fatalf("connector_needs=%v", m.ConnectorNeeds)
	}
	if m.AllowedTools == nil || len(*m.AllowedTools) != 1 || (*m.AllowedTools)[0] != "search" {
		t.Fatalf("allowed_tools=%#v", m.AllowedTools)
	}
	if !m.PolicyFailClosed {
		t.Fatal("expected policy_fail_closed=true")
	}
	if m.Principal == nil || m.Principal.Identity != "alice" {
		t.Fatalf("principal=%#v", m.Principal)
	}
	if m.ParentRunID == nil || *m.ParentRunID != parent || m.Depth != 2 {
		t.Fatalf("a2a fields: parent=%v depth=%d", m.ParentRunID, m.Depth)
	}
}

func TestRunManifestToMetadataRoundTrip(t *testing.T) {
	m := buildRunManifest(runManifestInput{
		capturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		tenantID:   "t",
		agentID:    "a",
		agent: &models.Agent{
			Metadata: map[string]interface{}{
				"allowed_tools":     []string{},
				"allowed_tools_set": true,
			},
		},
	})
	meta := runManifestToMetadata(m)
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded models.RunManifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != runManifestSchemaVersion {
		t.Fatalf("schema_version=%d", decoded.SchemaVersion)
	}
	if decoded.AllowedTools == nil || len(*decoded.AllowedTools) != 0 {
		t.Fatalf("expected explicit empty allowlist, got %#v", decoded.AllowedTools)
	}
}

// TestTryServeCachedRun_StillGetsRunManifest is a regression test: a cache
// hit returns before createRunCtx's normal manifest-building code ever
// runs, so without its own manifest logic every cached run would silently
// have no run_manifest at all -- an inconsistent audit trail depending on
// whether a run happened to hit cache. tryServeCachedRun must build one too.
func TestTryServeCachedRun_StillGetsRunManifest(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	threadID := "idle-cache"
	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusIdle,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// UpsertAgent computes its own version from content history (a fresh
	// agent always starts at 1) -- the Version field set here is not what
	// ends up stored, so the assertion below checks against 1, not this.
	if err := store.UpsertAgent(ctx, &models.Agent{
		AgentID: "cached-manifest-agent", Name: "cached-manifest-agent",
		Metadata: map[string]interface{}{
			"cache_ttl_seconds": 60,
			"runner_kind":       "typescript-langgraphjs",
			"allowed_tools":     []interface{}{"lookup"},
			"allowed_tools_set": true,
		},
		Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"q":"cached"}`)
	key := computeCacheKey(tenant.FromContext(ctx), "cached-manifest-agent", input, nil)
	now := time.Now().UTC()
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey: key, AgentID: "cached-manifest-agent",
		Output:    map[string]interface{}{"ok": true},
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	agent, err := store.GetAgent(ctx, "cached-manifest-agent")
	if err != nil || agent == nil {
		t.Fatalf("GetAgent: %v", err)
	}
	run, hit, err := s.tryServeCachedRun(ctx, "run-cache-manifest", threadID, &models.RunCreate{
		AgentID: "cached-manifest-agent", Input: input,
	}, now, "cached-alias", agent)
	if err != nil {
		t.Fatalf("tryServeCachedRun: %v", err)
	}
	if !hit {
		t.Fatal("expected a cache hit")
	}
	if run.Metadata["cache_hit"] != true {
		t.Fatalf("expected cache_hit=true, got %#v", run.Metadata["cache_hit"])
	}
	if run.Metadata["requested_alias"] != "cached-alias" {
		t.Fatalf("expected requested_alias preserved on cache hit, got %#v", run.Metadata["requested_alias"])
	}
	rawManifest, ok := run.Metadata["run_manifest"]
	if !ok {
		t.Fatal("cache-hit run is missing run_manifest -- audit trail now depends on whether the run happened to hit cache")
	}
	metaBytes, _ := json.Marshal(rawManifest)
	var manifest models.RunManifest
	if err := json.Unmarshal(metaBytes, &manifest); err != nil {
		t.Fatalf("run_manifest on cache hit: %v", err)
	}
	if manifest.AgentID != "cached-manifest-agent" || manifest.AgentVersion != 1 {
		t.Fatalf("cache-hit manifest agent fields: id=%q version=%d", manifest.AgentID, manifest.AgentVersion)
	}
	if manifest.RunnerKind != "typescript-langgraphjs" {
		t.Fatalf("cache-hit manifest runner_kind=%q", manifest.RunnerKind)
	}
	if manifest.AllowedTools == nil || len(*manifest.AllowedTools) != 1 {
		t.Fatalf("cache-hit manifest allowed_tools=%#v", manifest.AllowedTools)
	}
	if manifest.RequestedAlias != "cached-alias" {
		t.Fatalf("cache-hit manifest requested_alias=%q", manifest.RequestedAlias)
	}
}

// TestCreateRunCtx_A2ADoesNotShortCircuitOnCache proves A2A delegation
// never takes the LLM cache fast-path: that path runs before depth/breadth
// checks and would produce a child run with no parent/depth at all.
func TestCreateRunCtx_A2ADoesNotShortCircuitOnCache(t *testing.T) {
	s, store := newLifecycleServer(t, inprocess.NewQueue())
	ctx := context.Background()
	parentThreadID := "parent-thread"
	childThreadID := "a2a-child-thread"
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: parentThreadID, Status: models.ThreadStatusBusy,
		Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	parentID := "parent-for-cache-a2a"
	if err := store.CreateRun(ctx, &models.Run{
		RunID: parentID, ThreadID: parentThreadID, AgentID: "parent", AssistantID: "parent",
		Status: models.RunStatusRunning, Depth: 0,
		Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, &models.Agent{
		AgentID: "cached-child", Name: "cached-child",
		Metadata:     map[string]interface{}{"cache_ttl_seconds": 60},
		Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"q":"same"}`)
	key := computeCacheKey(tenant.FromContext(ctx), "cached-child", input, nil)
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey: key, AgentID: "cached-child",
		Output:    map[string]interface{}{"ok": true},
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	parent := parentID
	run, assignment, err := s.createRunCtx(ctx, childThreadID, &models.RunCreate{
		AgentID: "cached-child", Input: input, ParentRunID: &parent,
	})
	if err != nil {
		t.Fatalf("createRunCtx: %v", err)
	}
	if assignment == nil {
		t.Fatal("A2A child must not cache-short-circuit — expected a normal dispatch with assignment")
	}
	if run.Metadata["cache_hit"] == true {
		t.Fatal("A2A child must not be marked cache_hit")
	}
	if run.ParentRunID == nil || *run.ParentRunID != parentID {
		t.Fatalf("parent_run_id=%v, want %q", run.ParentRunID, parentID)
	}
	if run.Depth != 1 {
		t.Fatalf("depth=%d, want 1", run.Depth)
	}
	if _, ok := run.Metadata["run_manifest"]; !ok {
		t.Fatal("A2A child missing run_manifest")
	}
}

func TestRunManifestToRaw(t *testing.T) {
	m := buildRunManifest(runManifestInput{
		capturedAt: time.Now(),
		tenantID:   "t",
		agentID:    "a",
	})
	raw := runManifestToRaw(m)
	if len(raw) == 0 {
		t.Fatal("expected non-empty raw JSON")
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe["agent_id"] != "a" {
		t.Fatalf("raw agent_id=%v", probe["agent_id"])
	}
}
