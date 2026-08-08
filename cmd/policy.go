package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
)

// policyGrantLister is implemented by SQL stores that persist Admin overlays.
type policyGrantLister interface {
	ListPolicyGrants(ctx context.Context) ([]*models.PolicyGrant, error)
}

// initPolicy loads langgraph.json's "policy" section (first file) and
// builds an Engine. Returns (nil, false) when absent/empty (V1 open).
// When policy.siem is set, registers an async WebhookSink on dispatcher
// for policy_decision events and attaches it as the Engine's Exporter.
// runEvents is true by default (tool_auth RunEvents on denials) unless
// policy.run_events is explicitly false.
func initPolicy(configPath string, store state.Store, dispatcher *hooks.Dispatcher) (eng *policy.Engine, runEvents bool) {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil, false
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Policy == nil {
		return nil, false
	}
	p := cfg.Policy

	var overlays []policy.Grant
	if lister, ok := store.(policyGrantLister); ok {
		if rows, err := lister.ListPolicyGrants(context.Background()); err != nil {
			slog.Warn("policy: load DB grants failed", "error", err)
		} else {
			for _, g := range rows {
				if g == nil {
					continue
				}
				og := policy.Grant{
					ID: g.ID, TenantID: g.TenantID, AgentID: g.AgentID, Connector: g.Connector,
				}
				if g.Tools != nil {
					og.Tools = &policy.ToolFilter{Allow: g.Tools.Allow, Deny: g.Tools.Deny}
				}
				overlays = append(overlays, og)
			}
		}
	}

	hasWebhook := p.Webhook != nil && p.Webhook.URL != ""
	// Any non-nil "policy" section enables the engine — including
	// "policy": {} for Admin-API-only grants (fail-closed until overlays).

	var auditor policy.Auditor
	auditOn := true
	if p.Audit != nil {
		auditOn = *p.Audit
	}
	if auditOn {
		if aw, ok := store.(policy.AuditStore); ok {
			auditor = policy.NewStoreAuditor(aw)
		} else {
			slog.Warn("policy: audit writes enabled but state backend has no durable audit store — decisions will not be persisted (Mongo; use Postgres/MySQL/SQLite for the trail)")
		}
	}

	pcfg := policy.Config{
		DefaultEffect: p.DefaultEffect,
		FailClosed:    p.FailClosed,
		Auditor:       auditor,
		Overlays:      overlays,
		ForceEnable:   true,
	}
	if p.CacheTTLMS > 0 {
		pcfg.CacheTTL = time.Duration(p.CacheTTLMS) * time.Millisecond
	}
	for _, g := range p.Grants {
		grant := policy.Grant{
			ID:        g.ID,
			TenantID:  g.TenantID,
			AgentID:   g.AgentID,
			Connector: g.Connector,
		}
		if g.Tools != nil {
			grant.Tools = &policy.ToolFilter{Allow: g.Tools.Allow, Deny: g.Tools.Deny}
		}
		pcfg.Grants = append(pcfg.Grants, grant)
	}
	for _, r := range p.MandatoryHITL {
		pcfg.MandatoryHITL = append(pcfg.MandatoryHITL, policy.MandatoryHITLRule{
			ID:        r.ID,
			TenantID:  r.TenantID,
			AgentID:   r.AgentID,
			Connector: r.Connector,
			Tools:     append([]string(nil), r.Tools...),
		})
	}
	if hasWebhook {
		wc := &policy.WebhookConfig{
			URL:    p.Webhook.URL,
			Secret: p.Webhook.Secret,
		}
		if p.Webhook.TimeoutMS > 0 {
			wc.Timeout = time.Duration(p.Webhook.TimeoutMS) * time.Millisecond
		}
		pcfg.Webhook = wc
	}

	eng = policy.New(pcfg)
	if eng == nil {
		return nil, false
	}

	runEvents = true
	if p.RunEvents != nil {
		runEvents = *p.RunEvents
	}

	siemOn := false
	if p.SIEM != nil && p.SIEM.URL != "" && dispatcher != nil {
		sink := hooks.NewWebhookSink(hooks.WebhookConfig{
			URL:    p.SIEM.URL,
			Secret: p.SIEM.Secret,
		}, store)
		dispatcher.Register(sink, hooks.PolicyDecision)
		eng.SetExporter(&siemExporter{d: dispatcher})
		siemOn = true
		slog.Info("policy: SIEM webhook registered", "url", p.SIEM.URL)
	}

	slog.Info("policy: enabled",
		"grants", len(pcfg.Grants),
		"overlays", len(overlays),
		"mandatory_hitl", len(pcfg.MandatoryHITL),
		"webhook", pcfg.Webhook != nil,
		"default_effect", pcfg.DefaultEffect,
		"audit", auditor != nil,
		"siem", siemOn,
		"run_events", runEvents,
	)
	return eng, runEvents
}

// siemExporter fans Decide results onto the shared hooks.Dispatcher
// without blocking the connector/MCP path.
type siemExporter struct {
	d *hooks.Dispatcher
}

func (s *siemExporter) ExportPolicyDecision(_ context.Context, in policy.PolicyInput, dec policy.PolicyDecision) {
	if s == nil || s.d == nil {
		return
	}
	s.d.Dispatch(hooks.Event{
		Type:     hooks.PolicyDecision,
		RunID:    in.RunID,
		AgentID:  in.AgentID,
		TenantID: in.TenantID,
		Data: map[string]interface{}{
			"stage":       in.Stage,
			"effect":      dec.Effect,
			"reason":      dec.Reason,
			"reason_code": dec.ReasonCode,
			"rule_id":     dec.RuleID,
			"latency_ms":  dec.LatencyMs,
			"connector":   in.Connector,
			"tool":        in.Tool,
			"generation":  in.Generation,
			"principal":   in.Principal,
		},
		Timestamp: time.Now().UTC(),
	})
}
