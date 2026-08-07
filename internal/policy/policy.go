// Package policy is the control-plane PolicyProvider: static grants and
// an optional sync webhook gate for connector session mint and MCP
// tools/call. Absent config preserves V1 open behavior; when any grant
// or webhook is configured, unmatched requests default to deny.
package policy

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Stages evaluated today. run.create / a2a stay on hooks.Gate (Phase 3).
const (
	StageConnectorSession = "connector.session"
	StageToolCall         = "tool.call"
)

// Effects returned by Decide.
const (
	EffectAllow   = "allow"
	EffectDeny    = "deny"
	EffectPending = "pending" // reserved; HITL lands in Phase 2
)

// Stable reason codes for denials / failures.
const (
	ReasonPolicyDeny           = "policy_deny"
	ReasonPolicyNoGrant        = "policy_no_grant"
	ReasonPolicyToolDenied     = "policy_tool_denied"
	ReasonPolicyWebhookDeny    = "policy_webhook_deny"
	ReasonPolicyWebhookFailed  = "policy_webhook_failed"
	ReasonPolicyDefaultDeny    = "policy_default_deny"
	ReasonPolicyMissingBinding = "policy_missing_binding"
)

// PolicyInput is the decision context for one gate check.
type PolicyInput struct {
	Stage      string
	TenantID   string
	Principal  string
	AgentID    string
	RunID      string
	Generation int64
	Connector  string
	Tool       string
	ArgsMeta   map[string]any
	ArgsDigest string
	Claims     map[string]any
	Labels     map[string]string
}

// PolicyDecision is the allow/deny answer for one Decide call.
type PolicyDecision struct {
	Effect     string
	Reason     string
	ReasonCode string
	RuleID     string
	LatencyMs  int
}

// Provider evaluates one policy layer.
type Provider interface {
	Decide(ctx context.Context, in PolicyInput) (PolicyDecision, error)
}

// Auditor persists a decision. Nil/no-op is fine (Compatible backends).
type Auditor interface {
	WritePolicyDecision(ctx context.Context, in PolicyInput, dec PolicyDecision) error
}

// Engine chains static grants then optional webhook. First deny wins.
// When Enabled() is false, callers must skip Decide (V1 open).
type Engine struct {
	static        *Static
	webhook       *Webhook
	auditor       Auditor
	cache         *decisionCache
	defaultEffect string
	failClosed    bool
}

// Config builds an Engine from langgraph.json's policy section.
type Config struct {
	DefaultEffect string // "allow" | "deny"; default "deny" when enabled
	FailClosed    *bool  // webhook errors; default true
	CacheTTL      time.Duration
	Grants        []Grant
	Webhook       *WebhookConfig
	Auditor       Auditor
}

// New returns an Engine, or nil when nothing is configured (V1 open).
func New(cfg Config) *Engine {
	hasGrants := len(cfg.Grants) > 0
	hasWebhook := cfg.Webhook != nil && strings.TrimSpace(cfg.Webhook.URL) != ""
	if !hasGrants && !hasWebhook {
		return nil
	}
	def := strings.ToLower(strings.TrimSpace(cfg.DefaultEffect))
	if def != EffectAllow {
		def = EffectDeny
	}
	failClosed := true
	if cfg.FailClosed != nil {
		failClosed = *cfg.FailClosed
	}
	e := &Engine{
		defaultEffect: def,
		failClosed:    failClosed,
		auditor:       cfg.Auditor,
	}
	if hasGrants {
		e.static = NewStatic(cfg.Grants)
	}
	if hasWebhook {
		e.webhook = NewWebhook(*cfg.Webhook)
	}
	if cfg.CacheTTL > 0 {
		e.cache = newDecisionCache(cfg.CacheTTL)
	}
	return e
}

// Enabled reports whether any policy layer is active.
func (e *Engine) Enabled() bool {
	return e != nil && (e.static != nil || e.webhook != nil)
}

// Decide evaluates static grants then webhook. Call only when Enabled().
func (e *Engine) Decide(ctx context.Context, in PolicyInput) PolicyDecision {
	start := time.Now()
	dec := e.decide(ctx, in)
	dec.LatencyMs = int(time.Since(start).Milliseconds())
	if e.auditor != nil {
		if err := e.auditor.WritePolicyDecision(ctx, in, dec); err != nil {
			slog.Warn("policy: audit write failed", "error", err, "stage", in.Stage, "run_id", in.RunID)
		}
	}
	return dec
}

func (e *Engine) decide(ctx context.Context, in PolicyInput) PolicyDecision {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.AgentID) == "" {
		return PolicyDecision{
			Effect:     EffectDeny,
			Reason:     "policy requires tenant_id and agent_id from the run assignment",
			ReasonCode: ReasonPolicyMissingBinding,
		}
	}

	if e.cache != nil {
		if hit, ok := e.cache.get(in); ok {
			return hit
		}
	}

	var staticAllow *PolicyDecision
	if e.static != nil {
		dec := e.static.decide(in)
		if dec.Effect == EffectDeny {
			// No grant: honor default_effect when that is the only reason.
			if dec.ReasonCode == ReasonPolicyNoGrant && e.defaultEffect == EffectAllow && e.webhook == nil {
				out := PolicyDecision{Effect: EffectAllow, Reason: "default allow (no matching grant)"}
				e.cachePut(in, out)
				return out
			}
			e.cachePut(in, dec)
			return dec
		}
		staticAllow = &dec
	}

	if e.webhook != nil {
		dec := e.webhook.Decide(ctx, in, e.failClosed)
		if dec.Effect != EffectAllow {
			e.cachePut(in, dec)
			return dec
		}
		if staticAllow != nil {
			dec.RuleID = staticAllow.RuleID
			dec.Reason = staticAllow.Reason
		}
		e.cachePut(in, dec)
		return dec
	}

	if staticAllow != nil {
		e.cachePut(in, *staticAllow)
		return *staticAllow
	}

	// Webhook-only path that somehow didn't return, or empty layers.
	out := PolicyDecision{
		Effect:     e.defaultEffect,
		Reason:     "no matching policy grant",
		ReasonCode: ReasonPolicyDefaultDeny,
	}
	if e.defaultEffect == EffectAllow {
		out.Reason = "default allow"
		out.ReasonCode = ""
	}
	e.cachePut(in, out)
	return out
}

func (e *Engine) cachePut(in PolicyInput, dec PolicyDecision) {
	if e.cache != nil {
		e.cache.put(in, dec)
	}
}
