package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ApprovalRequirements applies to held operations. It does not create an allow
// or require approval for a call that current policy already permits.
type ApprovalRequirements struct {
	RequiredApprovals int  `json:"required_approvals"`
	ExcludeRequester  bool `json:"exclude_requester"`
}

func (a ApprovalRequirements) Valid() bool {
	return a.RequiredApprovals >= 1 && a.RequiredApprovals <= 5
}

func (e *Engine) ApprovalRequirements() ApprovalRequirements {
	if e == nil {
		return ApprovalRequirements{RequiredApprovals: 1}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.approvalRequirements
}

// Revision fingerprints the effective configured policy. A webhook's remote
// rules are represented only by its operator-declared revision, not attested.
// HMAC credentials never enter this descriptor.
func (e *Engine) Revision() string {
	if e == nil {
		return ""
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var grants []Grant
	if e.static != nil {
		grants = e.static.grants
	}
	type webhookRevision struct {
		URL, Revision string
		Timeout       int64
	}
	var webhook *webhookRevision
	if e.webhook != nil {
		webhook = &webhookRevision{e.webhook.cfg.URL, e.webhook.cfg.Revision, int64(e.webhook.cfg.Timeout)}
	}
	descriptor := struct {
		DefaultEffect             string
		FailClosed, AuditRequired bool
		Grants                    []Grant
		Predicates                []Predicate
		MandatoryHITL             []MandatoryHITLRule
		Webhook                   *webhookRevision
		Approval                  ApprovalRequirements
	}{e.defaultEffect, e.failClosed, e.auditRequired, grants, e.predicates, e.mandatoryHITL, webhook, e.approvalRequirements}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return "policy-v1-" + hex.EncodeToString(digest[:])
}
