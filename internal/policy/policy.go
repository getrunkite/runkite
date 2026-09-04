// Package policy is the control-plane PolicyProvider: static grants and
// an optional sync webhook gate for connector session mint and MCP
// tools/call. Absent config preserves V1 open behavior; when any grant
// or webhook is configured, unmatched requests default to deny.
package policy

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Stages evaluated by Decide. Connector stages use static grants;
// run.create skips connector matching (authz + kill switches run in the
// admission Gate; optional webhook still applies here).
const (
	StageConnectorSession = "connector.session"
	StageToolCall         = "tool.call"
	StageRunCreate        = "run.create"
)

// Effects returned by Decide.
const (
	EffectAllow   = "allow"
	EffectDeny    = "deny"
	EffectPending = "pending" // connector HITL: Admin approve → one-shot retry
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
	ReasonPolicyPending        = "policy_pending"
	ReasonPolicyAdmissionDeny  = "policy_admission_deny"
	ReasonBreakGlass           = "break_glass"
	ReasonKillActive           = "kill_active"
	ReasonKillActivate         = "kill_activate"
	ReasonKillClear            = "kill_clear"
	ReasonAuthzDeny            = "authz_deny"
	ReasonBudgetExceeded       = "budget_exceeded"
	ReasonBudgetSoft           = "budget_soft"
	ReasonBudgetAlert          = "budget_alert"    // soft_pct approach (under hard cap)
	ReasonBudgetKill           = "budget_kill"     // cancel_inflight on hard breach
	ReasonUsageUnpriced        = "usage_unpriced"  // tokens > 0, no cost_usd, non-empty pricebook missing this model
	ReasonUsageUnmetered       = "usage_unmetered" // runner saw an AI reply but extracted zero usage in any recognized shape
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

// Exporter fans a decision out asynchronously (SIEM webhook, etc.).
// Must not block; nil is a no-op.
type Exporter interface {
	ExportPolicyDecision(ctx context.Context, in PolicyInput, dec PolicyDecision)
}

// Engine chains static grants then optional webhook. First deny wins.
// When Enabled() is false, callers must skip Decide (V1 open).
type Engine struct {
	mu       sync.RWMutex
	baseline []Grant // immutable deployment defaults from langgraph.json
	overlays []Grant // durable Admin/DB grants; win on same key
	static   *Static
	webhook  *Webhook
	// mandatoryHITL* — config baseline + SQL overlays; merged into mandatoryHITL.
	mandatoryHITLBaseline []MandatoryHITLRule
	mandatoryHITLOverlays []MandatoryHITLRule
	mandatoryHITL         []MandatoryHITLRule // force allow → pending on tool.call
	auditor               Auditor
	exporter              Exporter
	cache                 *decisionCache
	defaultEffect         string
	failClosed            bool
}

// Config builds an Engine from langgraph.json's policy section.
type Config struct {
	DefaultEffect string // "allow" | "deny"; default "deny" when enabled
	FailClosed    *bool  // webhook errors; default true
	CacheTTL      time.Duration
	Grants        []Grant // deployment defaults (baseline)
	Overlays      []Grant // DB grants loaded at startup
	// MandatoryHITL forces matching tool.call allows to pending (config-only).
	MandatoryHITL []MandatoryHITLRule
	Webhook       *WebhookConfig
	Auditor       Auditor
	Exporter      Exporter
	// ForceEnable builds an engine even with no grants/webhook (empty
	// "policy": {} for Admin-API-only grant management).
	ForceEnable bool
}

// New returns an Engine, or nil when nothing is configured (V1 open).
func New(cfg Config) *Engine {
	hasBaseline := len(cfg.Grants) > 0
	hasOverlays := len(cfg.Overlays) > 0
	hasWebhook := cfg.Webhook != nil && strings.TrimSpace(cfg.Webhook.URL) != ""
	hasMandatory := len(cfg.MandatoryHITL) > 0
	if !hasBaseline && !hasOverlays && !hasWebhook && !hasMandatory && !cfg.ForceEnable {
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
		defaultEffect:         def,
		failClosed:            failClosed,
		auditor:               cfg.Auditor,
		exporter:              cfg.Exporter,
		baseline:              append([]Grant(nil), cfg.Grants...),
		mandatoryHITLBaseline: append([]MandatoryHITLRule(nil), cfg.MandatoryHITL...),
	}
	if hasWebhook {
		e.webhook = NewWebhook(*cfg.Webhook)
	}
	if cfg.CacheTTL > 0 {
		e.cache = newDecisionCache(cfg.CacheTTL)
	}
	e.applyOverlaysLocked(cfg.Overlays)
	e.applyMandatoryHITLOverlaysLocked(nil)
	return e
}

// SetExporter attaches (or clears) the async SIEM/export sink after New.
func (e *Engine) SetExporter(exp Exporter) {
	if e == nil {
		return
	}
	e.exporter = exp
}

// ReplaceOverlays swaps durable DB grants and rebuilds the static layer.
// Overlay rows win over baseline on (tenant_id, agent_id, connector).
func (e *Engine) ReplaceOverlays(overlays []Grant) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyOverlaysLocked(overlays)
}

func (e *Engine) applyOverlaysLocked(overlays []Grant) {
	e.overlays = append([]Grant(nil), overlays...)
	merged := mergeGrants(e.baseline, e.overlays)
	if len(merged) == 0 {
		e.static = nil
	} else {
		e.static = NewStatic(merged)
	}
	e.cache.clear()
}

// ReplaceMandatoryHITL swaps durable SQL mandatory-HITL overlays and
// rebuilds the active rule list. Overlay rows win over baseline on
// (tenant_id, agent_id, connector).
func (e *Engine) ReplaceMandatoryHITL(overlays []MandatoryHITLRule) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyMandatoryHITLOverlaysLocked(overlays)
}

func (e *Engine) applyMandatoryHITLOverlaysLocked(overlays []MandatoryHITLRule) {
	e.mandatoryHITLOverlays = append([]MandatoryHITLRule(nil), overlays...)
	e.mandatoryHITL = mergeMandatoryHITL(e.mandatoryHITLBaseline, e.mandatoryHITLOverlays)
	if e.cache != nil {
		e.cache.clear()
	}
}

// mergeMandatoryHITL unions baseline and overlays; overlays win on the same key.
// Empty agent_id is a valid key (whole-tenant rule), unlike connector grants.
func mergeMandatoryHITL(baseline, overlays []MandatoryHITLRule) []MandatoryHITLRule {
	type key struct{ t, a, c string }
	out := make([]MandatoryHITLRule, 0, len(baseline)+len(overlays))
	idx := map[key]int{}
	add := func(r MandatoryHITLRule) {
		k := key{strings.TrimSpace(r.TenantID), strings.TrimSpace(r.AgentID), strings.TrimSpace(r.Connector)}
		if k.t == "" || k.c == "" {
			return
		}
		r.Tools = append([]string(nil), r.Tools...)
		if i, ok := idx[k]; ok {
			out[i] = r
			return
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	for _, r := range baseline {
		add(r)
	}
	for _, r := range overlays {
		add(r)
	}
	return out
}

// mergeGrants unions baseline and overlays; overlays win on the same key.
func mergeGrants(baseline, overlays []Grant) []Grant {
	type key struct{ t, a, c string }
	out := make([]Grant, 0, len(baseline)+len(overlays))
	idx := map[key]int{}
	for _, g := range baseline {
		k := key{strings.TrimSpace(g.TenantID), strings.TrimSpace(g.AgentID), strings.TrimSpace(g.Connector)}
		if k.t == "" || k.a == "" || k.c == "" {
			continue
		}
		if i, ok := idx[k]; ok {
			out[i] = g
			continue
		}
		idx[k] = len(out)
		out = append(out, g)
	}
	for _, g := range overlays {
		k := key{strings.TrimSpace(g.TenantID), strings.TrimSpace(g.AgentID), strings.TrimSpace(g.Connector)}
		if k.t == "" || k.a == "" || k.c == "" {
			continue
		}
		if i, ok := idx[k]; ok {
			out[i] = g
			continue
		}
		idx[k] = len(out)
		out = append(out, g)
	}
	return out
}

// Enabled reports whether the policy engine is attached. An empty
// ForceEnable engine (no grants/webhook yet) is still enabled — Decide
// applies default_effect until Admin overlays or config grants appear.
func (e *Engine) Enabled() bool {
	return e != nil
}

// Decide evaluates static grants then webhook. Call only when Enabled().
// Every call (including cache hits) emits an OTel span event, writes
// audit when configured, and fans out to Exporter (SIEM) when set.
func (e *Engine) Decide(ctx context.Context, in PolicyInput) PolicyDecision {
	start := time.Now()
	dec := e.decide(ctx, in)
	dec = e.applyMandatoryHITL(in, dec)
	dec.LatencyMs = int(time.Since(start).Milliseconds())
	recordPolicySpanEvent(ctx, in, dec)
	if e.auditor != nil {
		if err := e.auditor.WritePolicyDecision(ctx, in, dec); err != nil {
			slog.Warn("policy: audit write failed", "error", err, "stage", in.Stage, "run_id", in.RunID)
		}
	}
	if e.exporter != nil {
		e.exporter.ExportPolicyDecision(ctx, in, dec)
	}
	return dec
}

func recordPolicySpanEvent(ctx context.Context, in PolicyInput, dec PolicyDecision) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("policy.stage", in.Stage),
		attribute.String("policy.effect", dec.Effect),
		attribute.Int("policy.latency_ms", dec.LatencyMs),
	}
	if dec.ReasonCode != "" {
		attrs = append(attrs, attribute.String("policy.reason_code", dec.ReasonCode))
	}
	if dec.RuleID != "" {
		attrs = append(attrs, attribute.String("policy.rule_id", dec.RuleID))
	}
	if in.TenantID != "" {
		attrs = append(attrs, attribute.String("tenant.id", in.TenantID))
	}
	if in.RunID != "" {
		attrs = append(attrs, attribute.String("run.id", in.RunID))
	}
	if in.AgentID != "" {
		attrs = append(attrs, attribute.String("agent.id", in.AgentID))
	}
	if in.Connector != "" {
		attrs = append(attrs, attribute.String("connector", in.Connector))
	}
	if in.Tool != "" {
		attrs = append(attrs, attribute.String("tool", in.Tool))
	}
	span.AddEvent("policy.decide", trace.WithAttributes(attrs...))
}

func (e *Engine) decide(ctx context.Context, in PolicyInput) PolicyDecision {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.AgentID) == "" {
		return PolicyDecision{
			Effect:     EffectDeny,
			Reason:     "policy requires tenant_id and agent_id from the run assignment",
			ReasonCode: ReasonPolicyMissingBinding,
		}
	}

	e.mu.RLock()
	static := e.static
	webhook := e.webhook
	defaultEffect := e.defaultEffect
	failClosed := e.failClosed
	e.mu.RUnlock()

	if e.cache != nil {
		if hit, ok := e.cache.get(in); ok {
			return hit
		}
	}

	// run.create is not keyed by connector — skip static grants so a
	// connector-only policy section does not deny every run create.
	if in.Stage == StageRunCreate {
		return e.decideRunCreate(ctx, in, webhook, failClosed)
	}

	var staticAllow *PolicyDecision
	if static != nil {
		dec := static.decide(in)
		if dec.Effect == EffectDeny {
			// No grant: honor default_effect when that is the only reason.
			if dec.ReasonCode == ReasonPolicyNoGrant && defaultEffect == EffectAllow && webhook == nil {
				out := PolicyDecision{Effect: EffectAllow, Reason: "default allow (no matching grant)"}
				e.cachePut(in, out)
				return out
			}
			e.cachePut(in, dec)
			return dec
		}
		staticAllow = &dec
	}

	if webhook != nil {
		dec := webhook.Decide(ctx, in, failClosed)
		if dec.Effect != EffectAllow {
			// Never cache pending — approve + retry must re-evaluate / see capability.
			if dec.Effect != EffectPending {
				e.cachePut(in, dec)
			}
			if dec.Effect == EffectPending && dec.ReasonCode == "" {
				dec.ReasonCode = ReasonPolicyPending
			}
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
		Effect:     defaultEffect,
		Reason:     "no matching policy grant",
		ReasonCode: ReasonPolicyDefaultDeny,
	}
	if defaultEffect == EffectAllow {
		out.Reason = "default allow"
		out.ReasonCode = ""
	}
	e.cachePut(in, out)
	return out
}

// decideRunCreate skips connector grants. Optional webhook can still deny;
// pending is treated as deny (HITL is connector-only).
func (e *Engine) decideRunCreate(ctx context.Context, in PolicyInput, webhook *Webhook, failClosed bool) PolicyDecision {
	if webhook != nil {
		dec := webhook.Decide(ctx, in, failClosed)
		if dec.Effect == EffectPending {
			out := PolicyDecision{
				Effect:     EffectDeny,
				Reason:     "run.create does not support pending",
				ReasonCode: ReasonPolicyAdmissionDeny,
				RuleID:     dec.RuleID,
			}
			e.cachePut(in, out)
			return out
		}
		if dec.Effect != EffectAllow {
			if dec.ReasonCode == "" {
				dec.ReasonCode = ReasonPolicyAdmissionDeny
			}
			e.cachePut(in, dec)
			return dec
		}
		e.cachePut(in, dec)
		return dec
	}
	out := PolicyDecision{
		Effect: EffectAllow,
		Reason: "run.create admitted",
	}
	e.cachePut(in, out)
	return out
}

func (e *Engine) cachePut(in PolicyInput, dec PolicyDecision) {
	if e.cache != nil {
		e.cache.put(in, dec)
	}
}
