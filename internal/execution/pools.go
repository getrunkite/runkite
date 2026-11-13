// Package execution defines operator-owned assignment routing boundaries.
package execution

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const PoolKindPrefix = "pool-"

var poolName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Pool grants one independently authenticated fleet an exact set of tenants
// and agents. It is not a unique workload identity or process sandbox.
type Pool struct {
	RunnerKind string   `json:"runner_kind"`
	Tenants    []string `json:"tenants"`
	Agents     []string `json:"agents"`
}

// Catalog is immutable after construction and never contains credentials.
type Catalog struct{ pools map[string]Pool }

func IsPoolKind(kind string) bool { return strings.HasPrefix(kind, PoolKindPrefix) }

func Kind(name string) (string, error) {
	if !poolName.MatchString(name) {
		return "", fmt.Errorf("invalid execution pool name")
	}
	return PoolKindPrefix + name, nil
}

func NewCatalog(config map[string]Pool) (*Catalog, error) {
	c := &Catalog{pools: make(map[string]Pool, len(config))}
	for name, p := range config {
		kind, err := Kind(name)
		if err != nil {
			return nil, err
		}
		if !poolName.MatchString(p.RunnerKind) || IsPoolKind(p.RunnerKind) {
			return nil, fmt.Errorf("execution pool %q requires a non-pool runner_kind", name)
		}
		for _, list := range [][]string{p.Tenants, p.Agents} {
			if len(list) == 0 {
				return nil, fmt.Errorf("execution pool %q requires explicit tenants and agents", name)
			}
			for _, id := range list {
				if id == "" || id == "*" || strings.TrimSpace(id) != id || !utf8.ValidString(id) || strings.ContainsFunc(id, unicode.IsControl) {
					return nil, fmt.Errorf("execution pool %q has an invalid authority identifier", name)
				}
			}
		}
		p.Tenants = slices.Clone(p.Tenants)
		p.Agents = slices.Clone(p.Agents)
		c.pools[kind] = p
	}
	return c, nil
}

func (c *Catalog) Lookup(kind string) (Pool, bool) {
	if c == nil {
		return Pool{}, false
	}
	p, ok := c.pools[kind]
	p.Tenants, p.Agents = slices.Clone(p.Tenants), slices.Clone(p.Agents)
	return p, ok
}

func (c *Catalog) Kinds() []string {
	var out []string
	if c != nil {
		for kind := range c.pools {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}

// AllowsRoute leaves existing trusted fleets unchanged. Reserved pool kinds
// require an explicit catalog entry and exact membership at every boundary.
func (c *Catalog) AllowsRoute(kind, tenant, agent string) bool {
	if !IsPoolKind(kind) {
		// API-created/edited agent metadata cannot route a pool-scoped agent
		// back onto an unrestricted framework queue.
		if c != nil {
			for _, pool := range c.pools {
				if slices.Contains(pool.Tenants, tenant) && slices.Contains(pool.Agents, agent) {
					return false
				}
			}
		}
		return true
	}
	p, ok := c.Lookup(kind)
	return ok && slices.Contains(p.Tenants, tenant) && slices.Contains(p.Agents, agent)
}

func (c *Catalog) Select(name, framework string, agents []string) (string, error) {
	kind, err := Kind(name)
	if err != nil {
		return "", err
	}
	p, ok := c.Lookup(kind)
	if !ok || p.RunnerKind != framework {
		return "", fmt.Errorf("execution pool is unknown or has a different runner_kind")
	}
	for _, agent := range agents {
		if !slices.Contains(p.Agents, agent) {
			return "", fmt.Errorf("agent is not allowed in execution pool")
		}
	}
	return kind, nil
}
