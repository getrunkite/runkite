package policy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/hooks"
)

// WebhookConfig is the sync policy webhook (WebhookGate-shaped, not
// the async Dispatcher/Sink path).
type WebhookConfig struct {
	URL     string
	Secret  string
	Timeout time.Duration
}

// Webhook POSTs a decision request and parses allow/deny.
type Webhook struct {
	cfg    WebhookConfig
	client *http.Client
}

// NewWebhook builds a webhook provider. Timeout defaults to hooks.DefaultPreflightTimeout.
func NewWebhook(cfg WebhookConfig) *Webhook {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = hooks.DefaultPreflightTimeout
	}
	return &Webhook{
		cfg: cfg,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

type webhookRequest struct {
	Type       string         `json:"type"`
	Stage      string         `json:"stage"`
	TenantID   string         `json:"tenant_id"`
	Principal  string         `json:"principal,omitempty"`
	AgentID    string         `json:"agent_id"`
	RunID      string         `json:"run_id,omitempty"`
	Generation int64          `json:"generation,omitempty"`
	Connector  string         `json:"connector,omitempty"`
	Tool       string         `json:"tool,omitempty"`
	Timestamp  string         `json:"timestamp"`
	Data       map[string]any `json:"data,omitempty"`
}

type webhookResponse struct {
	Allow      *bool  `json:"allow"`
	Effect     string `json:"effect,omitempty"` // optional: allow|deny|pending
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	RuleID     string `json:"rule_id,omitempty"`
}

// Decide calls the webhook. failClosed=true → errors become deny.
func (w *Webhook) Decide(ctx context.Context, in PolicyInput, failClosed bool) PolicyDecision {
	reqBody := webhookRequest{
		Type:       "policy.decide",
		Stage:      in.Stage,
		TenantID:   in.TenantID,
		Principal:  in.Principal,
		AgentID:    in.AgentID,
		RunID:      in.RunID,
		Generation: in.Generation,
		Connector:  in.Connector,
		Tool:       in.Tool,
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Data: map[string]any{
			"connector": in.Connector,
			"tool":      in.Tool,
			"identity":  in.Principal,
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return webhookFail(failClosed, "failed to marshal policy webhook request", ReasonPolicyWebhookFailed)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return webhookFail(failClosed, err.Error(), ReasonPolicyWebhookFailed)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if w.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.cfg.Secret))
		mac.Write(payload)
		httpReq.Header.Set("X-Runkite-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.client.Do(httpReq)
	if err != nil {
		return webhookFail(failClosed, fmt.Sprintf("policy webhook request failed: %v", err), ReasonPolicyWebhookFailed)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := string(bytes.TrimSpace(body))
		if reason == "" {
			reason = fmt.Sprintf("policy webhook HTTP %d", resp.StatusCode)
		}
		return webhookFail(failClosed, reason, ReasonPolicyWebhookFailed)
	}

	var parsed webhookResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return webhookFail(failClosed, "policy webhook response is not valid JSON", ReasonPolicyWebhookFailed)
	}

	effect := parsed.Effect
	if effect == "" && parsed.Allow != nil {
		if *parsed.Allow {
			effect = EffectAllow
		} else {
			effect = EffectDeny
		}
	}
	if effect == "" {
		return webhookFail(failClosed, "policy webhook response missing allow/effect", ReasonPolicyWebhookFailed)
	}

	code := parsed.ReasonCode
	if code == "" {
		switch effect {
		case EffectDeny:
			code = ReasonPolicyWebhookDeny
		case EffectPending:
			code = ReasonPolicyPending
		}
	}
	return PolicyDecision{
		Effect:     effect,
		Reason:     parsed.Reason,
		ReasonCode: code,
		RuleID:     parsed.RuleID,
	}
}

func webhookFail(failClosed bool, reason, code string) PolicyDecision {
	if !failClosed {
		return PolicyDecision{Effect: EffectAllow, Reason: "webhook error ignored (fail_open): " + reason}
	}
	return PolicyDecision{Effect: EffectDeny, Reason: reason, ReasonCode: code}
}
