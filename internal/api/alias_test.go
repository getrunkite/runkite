package api_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
)

func TestAliasResolver_NonAliasPassesThrough(t *testing.T) {
	r := api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent": {Targets: map[string]int{"my_agent_v1": 90, "my_agent_v2": 10}},
	})
	got, wasAlias := r.Resolve("some_other_agent")
	if wasAlias || got != "some_other_agent" {
		t.Fatalf("expected pass-through for non-alias, got (%q, %v)", got, wasAlias)
	}
}

func TestAliasResolver_NilResolverPassesThrough(t *testing.T) {
	var r *api.AliasResolver
	got, wasAlias := r.Resolve("my_agent")
	if wasAlias || got != "my_agent" {
		t.Fatalf("expected nil resolver to pass through, got (%q, %v)", got, wasAlias)
	}
}

func TestAliasResolver_WeightedDistributionApproximatesConfiguredSplit(t *testing.T) {
	r := api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent": {Targets: map[string]int{"v1": 90, "v2": 10}},
	})

	const trials = 20000
	counts := map[string]int{}
	for i := 0; i < trials; i++ {
		resolved, wasAlias := r.Resolve("my_agent")
		if !wasAlias {
			t.Fatalf("expected wasAlias=true for a configured alias")
		}
		counts[resolved]++
	}

	v1Frac := float64(counts["v1"]) / float64(trials)
	if v1Frac < 0.85 || v1Frac > 0.95 {
		t.Errorf("expected ~90%% of traffic to v1 (tolerance 85-95%%), got %.1f%% over %d trials: %+v", v1Frac*100, trials, counts)
	}
	if counts["v2"] == 0 {
		t.Error("expected at least some traffic to v2 over 20000 trials")
	}
}

func TestAliasResolver_WeightsNormalizedNotRequiredToSumTo100(t *testing.T) {
	// {"a": 1, "b": 1} should behave as an even 50/50 split, same as
	// {"a": 50, "b": 50} -- weights are relative, not percentages.
	r := api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"alias": {Targets: map[string]int{"a": 1, "b": 1}},
	})
	const trials = 10000
	counts := map[string]int{}
	for i := 0; i < trials; i++ {
		resolved, _ := r.Resolve("alias")
		counts[resolved]++
	}
	aFrac := float64(counts["a"]) / float64(trials)
	if aFrac < 0.4 || aFrac > 0.6 {
		t.Errorf("expected ~50%% split, got %.1f%%: %+v", aFrac*100, counts)
	}
}

func TestAliasResolver_ZeroOrNegativeWeightExcluded(t *testing.T) {
	r := api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"alias": {Targets: map[string]int{"a": 100, "b": 0, "c": -5}},
	})
	for i := 0; i < 100; i++ {
		resolved, _ := r.Resolve("alias")
		if resolved != "a" {
			t.Fatalf("expected only 'a' (nonzero weight) to ever be picked, got %q", resolved)
		}
	}
}
