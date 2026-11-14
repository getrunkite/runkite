package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionPoolConfigRequiresKnownMatchingFrameworkAndAgents(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		valid        bool
	}{
		{"valid", `{"graphs":{"finance":"agent.py:graph"},"execution_pool":"finance","execution_pools":{"finance":{"runner_kind":"python-langgraph","tenants":["acme"],"agents":["finance"]}}}`, true},
		{"unknown", `{"graphs":{},"execution_pool":"unknown"}`, false},
		{"reserved", `{"graphs":{},"runner_kind":"pool-finance"}`, false},
		{"framework", `{"graphs":{},"runner_kind":"typescript-ai-sdk","execution_pool":"finance","execution_pools":{"finance":{"runner_kind":"python-langgraph","tenants":["acme"],"agents":["finance"]}}}`, false},
		{"agent", `{"graphs":{"other":"agent.py:graph"},"execution_pool":"finance","execution_pools":{"finance":{"runner_kind":"python-langgraph","tenants":["acme"],"agents":["finance"]}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "langgraph.json")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadExecutionPools(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
