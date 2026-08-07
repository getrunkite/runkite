package main

import (
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
)

// initPolicy loads langgraph.json's "policy" section (first file) and
// builds an Engine. Returns nil when absent/empty (V1 open).
func initPolicy(configPath string, store state.Store) *policy.Engine {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Policy == nil {
		return nil
	}
	p := cfg.Policy
	if len(p.Grants) == 0 && (p.Webhook == nil || p.Webhook.URL == "") {
		return nil
	}

	var auditor policy.Auditor
	auditOn := true
	if p.Audit != nil {
		auditOn = *p.Audit
	}
	if auditOn {
		if pg, ok := store.(*pgstore.Store); ok {
			auditor = policy.NewStoreAuditor(pg)
		} else {
			slog.Warn("policy: audit writes enabled but state backend is not Postgres — decisions will not be persisted (Supported profile only in this release)")
		}
	}

	pcfg := policy.Config{
		DefaultEffect: p.DefaultEffect,
		FailClosed:    p.FailClosed,
		Auditor:       auditor,
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
	if p.Webhook != nil && p.Webhook.URL != "" {
		wc := &policy.WebhookConfig{
			URL:    p.Webhook.URL,
			Secret: p.Webhook.Secret,
		}
		if p.Webhook.TimeoutMS > 0 {
			wc.Timeout = time.Duration(p.Webhook.TimeoutMS) * time.Millisecond
		}
		pcfg.Webhook = wc
	}

	eng := policy.New(pcfg)
	if eng == nil {
		return nil
	}
	slog.Info("policy: enabled",
		"grants", len(pcfg.Grants),
		"webhook", pcfg.Webhook != nil,
		"default_effect", pcfg.DefaultEffect,
		"audit", auditor != nil,
	)
	return eng
}
