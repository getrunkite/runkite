package models

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/transport"
)

const (
	RunUsageSourceIsolated       = "isolated_workload"
	RunUsageUnknown              = "unknown"
	RunUsageReported             = "reported"
	MaxUsageTokens         int64 = 1<<53 - 1
)

// RunUsageReport is a cumulative report from one isolated logical execution.
// Counters are claims by the workload, not provider-verified billing records.
type RunUsageReport struct {
	SchemaVersion    int      `json:"schema_version"`
	Sequence         int64    `json:"sequence"`
	Status           string   `json:"status"`
	Complete         bool     `json:"complete"`
	PromptTokens     *int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64   `json:"completion_tokens,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	Model            string   `json:"model,omitempty"`
}

// RunUsageSnapshot is private durable run state. Neither run metadata nor a
// workload's output can replace this server-stamped record.
type RunUsageSnapshot struct {
	RunUsageReport
	Source             string                      `json:"source"`
	ReportedGeneration int64                       `json:"reported_generation"`
	ReportedAt         time.Time                   `json:"reported_at"`
	WorkloadOwner      *transport.WorkloadIdentity `json:"workload_owner,omitempty"`
}

func (r RunUsageReport) Validate() error {
	invalid := func() error { return fmt.Errorf("invalid cumulative usage report") }
	if r.SchemaVersion != 1 || r.Sequence < 1 || r.Sequence > 1_000_000 || !utf8.ValidString(r.Model) || len(r.Model) > 256 || strings.ContainsAny(r.Model, "\x00\r\n") {
		return invalid()
	}
	if r.Status == RunUsageUnknown {
		if r.Complete || r.PromptTokens != nil || r.CompletionTokens != nil || r.CostUSD != nil || r.Model != "" {
			return invalid()
		}
		return nil
	}
	if r.Status != RunUsageReported || r.PromptTokens == nil || r.CompletionTokens == nil {
		return invalid()
	}
	if *r.PromptTokens < 0 || *r.CompletionTokens < 0 || *r.PromptTokens > MaxUsageTokens || *r.CompletionTokens > MaxUsageTokens-*r.PromptTokens {
		return invalid()
	}
	if r.CostUSD != nil && (math.IsNaN(*r.CostUSD) || math.IsInf(*r.CostUSD, 0) || *r.CostUSD < 0 || *r.CostUSD > 1e12) {
		return invalid()
	}
	return nil
}

// Advances rejects reuse of an idempotency sequence with changed data, omitted
// known counters, regressions, and measurements after a declared final report.
func (r RunUsageReport) Advances(previous *RunUsageSnapshot) (bool, error) {
	if err := r.Validate(); err != nil {
		return false, err
	}
	if previous == nil {
		if r.Sequence != 1 {
			return false, fmt.Errorf("usage report sequence is not current")
		}
		return true, nil
	}
	old := previous.RunUsageReport
	if old.Validate() != nil || previous.Source != RunUsageSourceIsolated {
		return false, fmt.Errorf("invalid stored cumulative usage")
	}
	if r.Sequence == old.Sequence && reflect.DeepEqual(r, old) {
		return false, nil
	}
	if old.Complete || r.Sequence != old.Sequence+1 || (old.Status == RunUsageReported && r.Status != RunUsageReported) {
		return false, fmt.Errorf("usage report sequence is not current")
	}
	if old.Status == RunUsageReported && (*r.PromptTokens < *old.PromptTokens || *r.CompletionTokens < *old.CompletionTokens || r.Model != old.Model || (old.CostUSD != nil && (r.CostUSD == nil || *r.CostUSD < *old.CostUSD))) {
		return false, fmt.Errorf("cumulative usage regressed")
	}
	return true, nil
}

// OutputUsage preserves unknown usage and reported zero as different states.
// Complete only represents the workload's assertion, never trusted hard caps.
func (s *RunUsageSnapshot) OutputUsage() map[string]interface{} {
	u := map[string]interface{}{
		"reporting_status": s.Status, "reporting_source": RunUsageSourceIsolated,
		"complete": s.Complete, "sequence": s.Sequence, "reported_generation": s.ReportedGeneration,
		"unmetered": s.Status != RunUsageReported || !s.Complete,
	}
	if s.WorkloadOwner != nil {
		u["reported_workload"] = *s.WorkloadOwner
	}
	if s.Status == RunUsageReported {
		u["prompt_tokens"], u["completion_tokens"] = *s.PromptTokens, *s.CompletionTokens
		u["total_tokens"] = *s.PromptTokens + *s.CompletionTokens
		if s.Model != "" {
			u["model"] = s.Model
		}
		if s.CostUSD != nil {
			u["cost_usd"], u["cost_reported"] = *s.CostUSD, true
		}
	}
	return u
}
