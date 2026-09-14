package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLangGraphJSON_PolicyPredicates(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"policy": {
			"grants": [{"tenant_id":"acme","agent_id":"payroll","connector":"bank"}],
			"predicates": [{
				"id": "transfer-over-100",
				"tenant_id": "acme",
				"agent_id": "payroll",
				"connector": "bank",
				"tool": "transfer",
				"when": { "path": "amount", "op": "gt", "value": 100 },
				"effect": "pending",
				"reason": "approval required for transfers over 100",
				"reason_code": "predicate_amount"
			}]
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatalf("LoadLangGraphJSON: %v", err)
	}
	if cfg.Policy == nil || len(cfg.Policy.Predicates) != 1 {
		t.Fatalf("predicates = %+v", cfg.Policy)
	}
	p := cfg.Policy.Predicates[0]
	if p.ID != "transfer-over-100" || p.Tool != "transfer" || p.Effect != "pending" {
		t.Fatalf("predicate = %+v", p)
	}
	if p.When == nil || p.When.Path != "amount" || p.When.Op != "gt" {
		t.Fatalf("when = %+v", p.When)
	}
	if p.When.Value != float64(100) {
		t.Fatalf("value = %#v", p.When.Value)
	}
}
