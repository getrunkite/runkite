package execution

import "testing"

func TestPoolCatalogExactAuthorityAndImmutability(t *testing.T) {
	p := Pool{RunnerKind: "typescript-ai-sdk", Tenants: []string{"acme"}, Agents: []string{"finance"}}
	c, err := NewCatalog(map[string]Pool{"finance": p})
	if err != nil {
		t.Fatal(err)
	}
	p.Tenants[0] = "other"
	copy, _ := c.Lookup("pool-finance")
	copy.Agents[0] = "other"
	for _, tc := range []struct {
		kind, tenant, agent string
		allowed             bool
	}{
		{"pool-finance", "acme", "finance", true},
		{"pool-finance", "Acme", "finance", false},
		{"pool-finance", "acme", "other", false},
		{"pool-unknown", "acme", "finance", false},
		{"python-langgraph", "other", "other", true},
		{"python-langgraph", "acme", "finance", false},
	} {
		if got := c.AllowsRoute(tc.kind, tc.tenant, tc.agent); got != tc.allowed {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
	if _, err := c.Select("finance", "python-langgraph", []string{"finance"}); err == nil {
		t.Fatal("framework mismatch accepted")
	}
	if _, err := c.Select("finance", "typescript-ai-sdk", []string{"other"}); err == nil {
		t.Fatal("foreign graph accepted")
	}
}

func TestPoolCatalogRejectsOpenOrAmbiguousAuthority(t *testing.T) {
	for _, p := range []Pool{
		{RunnerKind: "python-langgraph", Agents: []string{"a"}},
		{RunnerKind: "python-langgraph", Tenants: []string{"*"}, Agents: []string{"a"}},
		{RunnerKind: "pool-other", Tenants: []string{"acme"}, Agents: []string{"a"}},
		{RunnerKind: "python-langgraph", Tenants: []string{" acme"}, Agents: []string{"a"}},
	} {
		if _, err := NewCatalog(map[string]Pool{"finance": p}); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
