package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPolicyAuditRequired(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		wantRequired bool
		wantErr      string
	}{
		{name: "default", policy: `{}`},
		{name: "required with default audit", policy: `{"audit_required":true}`, wantRequired: true},
		{name: "required with explicit audit", policy: `{"audit_required":true,"audit":true}`, wantRequired: true},
		{name: "optional audit disabled", policy: `{"audit_required":false,"audit":false}`},
		{name: "required audit disabled", policy: `{"audit_required":true,"audit":false}`, wantErr: "policy.audit_required requires policy.audit enabled"},
		{name: "wrong required type", policy: `{"audit_required":"true"}`, wantErr: "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "langgraph.json")
			body := `{"graphs":{"echo":"graph.py:graph"},"policy":` + tc.policy + `}`
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadLangGraphJSON(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || cfg.Policy == nil || cfg.Policy.AuditRequired != tc.wantRequired {
				t.Fatalf("config = %#v, error = %v", cfg, err)
			}
		})
	}
}
