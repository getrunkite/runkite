package models

import (
	"math"
	"testing"
)

func TestRunUsageRejectsContradictionsAndPreservesReportedZero(t *testing.T) {
	zero := int64(0)
	value := RunUsageReport{SchemaVersion: 1, Sequence: 1, Status: RunUsageReported, Complete: true, PromptTokens: &zero, CompletionTokens: &zero}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	output := (&RunUsageSnapshot{RunUsageReport: value, Source: RunUsageSourceIsolated}).OutputUsage()
	if output["unmetered"] != false || output["reporting_status"] != "reported" || output["prompt_tokens"] != int64(0) {
		t.Fatalf("reported zero lost: %+v", output)
	}
	nan := math.NaN()
	for _, change := range []func(*RunUsageReport){
		func(r *RunUsageReport) { r.SchemaVersion = 2 },
		func(r *RunUsageReport) { r.Status = RunUsageUnknown },
		func(r *RunUsageReport) { r.CompletionTokens = nil },
		func(r *RunUsageReport) { r.CostUSD = &nan },
		func(r *RunUsageReport) { n := int64(-1); r.PromptTokens = &n },
		func(r *RunUsageReport) {
			n := MaxUsageTokens
			r.PromptTokens = &n
			m := int64(1)
			r.CompletionTokens = &m
		},
	} {
		changed := value
		change(&changed)
		if changed.Validate() == nil {
			t.Fatalf("invalid report accepted: %+v", changed)
		}
	}
	unknown := &RunUsageSnapshot{RunUsageReport: RunUsageReport{SchemaVersion: 1, Sequence: 1, Status: RunUsageUnknown}, Source: RunUsageSourceIsolated}
	if unknown.OutputUsage()["unmetered"] != true || unknown.OutputUsage()["reporting_status"] != "unknown" {
		t.Fatal("unknown usage became a reported zero")
	}
}
