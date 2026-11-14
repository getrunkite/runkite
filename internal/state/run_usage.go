package state

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
)

type RunUsageBinding struct {
	RunCheckpointBinding
	RunIncarnationID string
	TenantID         string
}

// RunUsageStore is optional and SQL-only. Reports and terminal completion use
// the same run lock. Acknowledged cumulative reports survive process loss and
// settle through the existing terminal usage ledger exactly once per run.
type RunUsageStore interface {
	ReportRunUsage(context.Context, RunUsageBinding, models.RunUsageReport) (*models.RunUsageSnapshot, bool, error)
	GetRunUsage(context.Context, RunUsageBinding) (*models.RunUsageSnapshot, error)
}

// WithoutDurableUsage demotes reserved report provenance when no private SQL
// snapshot backs it. Native SDK counters retain their established convention.
func WithoutDurableUsage(completion *RunCompletion) *RunCompletion {
	claimed, ok := completion.Values["usage"].(map[string]interface{})
	if !ok {
		return completion
	}
	reserved := false
	for _, key := range []string{"reporting_source", "reporting_status", "reported_generation", "reported_workload", "cost_reported"} {
		_, present := claimed[key]
		reserved = reserved || present
	}
	if !reserved {
		return completion
	}
	copy := *completion
	copy.Values = make(map[string]interface{}, len(completion.Values))
	for key, value := range completion.Values {
		copy.Values[key] = value
	}
	copy.Values["usage"] = map[string]interface{}{"reporting_source": "unverified_output", "reporting_status": "unknown", "unmetered": true}
	return &copy
}
