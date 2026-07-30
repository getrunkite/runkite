// Package api: A/B deployment routing via agent aliases. A client-facing
// name resolves to one of several REAL, independently-registered
// agent_ids at run-creation time, weighted by langgraph.json's
// "agent_aliases" config -- e.g. rolling a new graph version out to 10%
// of traffic while the stable one keeps serving 90%, both deployed as
// ordinary agents with no special "candidate" flag or code path of
// their own.
package api

import (
	"math/rand"
	"sort"

	"github.com/sharanharsoor/runkite/internal/config"
)

// AliasResolver picks a real agent_id for a configured alias name,
// weighted-random per call. Not goroutine-unsafe to construct fresh per
// server (the underlying map is read-only after construction); Resolve
// itself is safe for concurrent use since math/rand's package-level
// functions are.
type AliasResolver struct {
	// targets[alias] is pre-sorted by agent_id for deterministic
	// iteration order (map iteration order is randomized in Go --
	// without sorting, two runs with the exact same rand seed could
	// still resolve differently depending on iteration order, making
	// tests and any "why did THIS request get routed there" debugging
	// non-reproducible).
	targets map[string][]weightedTarget
}

type weightedTarget struct {
	agentID string
	weight  int
}

// NewAliasResolver builds a resolver from langgraph.json's
// "agent_aliases" section. A nil/empty cfg produces a resolver whose
// Resolve is a pure pass-through (Resolve(x) == x, x, always) --
// aliasing is entirely opt-in.
func NewAliasResolver(cfg map[string]config.AgentAliasEntry) *AliasResolver {
	targets := make(map[string][]weightedTarget, len(cfg))
	for alias, entry := range cfg {
		list := make([]weightedTarget, 0, len(entry.Targets))
		for agentID, weight := range entry.Targets {
			if weight <= 0 {
				continue
			}
			list = append(list, weightedTarget{agentID: agentID, weight: weight})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].agentID < list[j].agentID })
		if len(list) > 0 {
			targets[alias] = list
		}
	}
	return &AliasResolver{targets: targets}
}

// Resolve returns (resolvedAgentID, wasAlias). wasAlias is false for
// any agentID that isn't a configured alias -- the caller passes it
// through unchanged, so a real agent_id that happens to not collide
// with any alias name behaves exactly as if aliasing didn't exist.
func (r *AliasResolver) Resolve(agentID string) (string, bool) {
	if r == nil {
		return agentID, false
	}
	list, ok := r.targets[agentID]
	if !ok {
		return agentID, false
	}

	total := 0
	for _, t := range list {
		total += t.weight
	}
	pick := rand.Intn(total) //nolint:gosec // routing weighting, not a security decision
	for _, t := range list {
		if pick < t.weight {
			return t.agentID, true
		}
		pick -= t.weight
	}
	// Unreachable given total is the exact sum of every weight above,
	// but a safe fallback (first target) rather than a panic if it
	// somehow were.
	return list[0].agentID, true
}
