package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func securityConfigStore(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSecurityCompletionStartupRefusesMissingDurability(t *testing.T) {
	db := securityConfigStore(t)
	for _, tc := range []struct {
		name, config string
		store        state.Store
		kind         string
	}{
		{"export without evidence", `{"policy":{"audit_required":true,"action_export":{"name":"audit","url":"https://audit.example/events"}}}`, db, "export"},
		{"export without required audit", `{"policy":{"action_evidence":true,"action_export":{"name":"audit","url":"https://audit.example/events"}}}`, db, "export"},
		{"export unsupported store", `{"policy":{"audit_required":true,"action_evidence":true,"action_export":{"name":"audit","url":"https://audit.example/events"}}}`, policyStoreWithoutAudit{db}, "export"},
		{"lease without evidence", `{"policy":{"audit_required":true},"credential_leases":{"keyring_file":"missing","bindings":[]}}`, db, "lease"},
		{"lease unsupported store", `{"policy":{"audit_required":true,"action_evidence":true},"credential_leases":{"keyring_file":"missing","bindings":[]}}`, policyStoreWithoutAudit{db}, "lease"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecurityCompletionConfig(t, t.TempDir(), tc.config)
			s := api.NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			var err error
			if tc.kind == "export" {
				err = initActionExport(t.Context(), path, tc.store, s)
			} else {
				err = initCredentialLeases(t.Context(), path, tc.store, s, connector.NewRegistry(nil))
			}
			if err == nil {
				t.Fatal("unsafe optional subsystem started")
			}
		})
	}
}

func TestCredentialStartupBindsConsumerWithoutIssuing(t *testing.T) {
	db := securityConfigStore(t)
	dir := t.TempDir()
	key, _ := json.Marshal(map[string]any{"active": "fixture", "keys": map[string]string{"fixture": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}})
	if err := os.WriteFile(filepath.Join(dir, "keyring.json"), key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("synthetic-fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := `{"policy":{"audit_required":true,"action_evidence":true},"credential_leases":{"keyring_file":"keyring.json","bindings":[{"id":"reporting","address":"https://vault.invalid","token_file":"token","mount":"database","role":"readonly","consumer_id":"database","max_ttl_seconds":60,"renew_seconds":30}]}}`
	path := writeSecurityCompletionConfig(t, dir, settings)
	makeRegistry := func(binding string) *connector.Registry {
		return connector.NewRegistry(map[string]connector.ConnectorConfig{"database": {Auth: connector.AuthConfig{Type: "vault_database", CredentialBinding: binding}, MCP: &connector.MCPConfig{URL: "https://consumer.invalid/mcp"}}})
	}
	s := api.NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	registry := makeRegistry("reporting")
	if err := initCredentialLeases(ctx, path, db, s, registry); err != nil {
		t.Fatal(err)
	}
	entry, err := registry.Get("database")
	if err != nil || !strings.HasPrefix(entry.Config.Auth.CredentialBindingRevision, "vault-db-v1-") {
		t.Fatal("consumer not bound to immutable provider contract")
	}
	// The .invalid addresses prove initialization neither issues a credential nor
	// probes the trusted executor. Each protected call performs those operations.
	if err := initCredentialLeases(t.Context(), path, db, s, makeRegistry("replacement")); err == nil {
		t.Fatal("unregistered binding accepted")
	}
	path = writeSecurityCompletionConfig(t, dir, `{}`)
	if err := initCredentialLeases(t.Context(), path, db, s, makeRegistry("reporting")); err == nil {
		t.Fatal("database connector without lifecycle accepted")
	}
}

func TestApprovalConfigurationBoundsFailStartup(t *testing.T) {
	db := securityConfigStore(t)
	for _, value := range []string{"-1", "6", "2147483647"} {
		path := writeSecurityCompletionConfig(t, t.TempDir(), `{"policy":{"approvals":{"required_approvals":`+value+`}}}`)
		if eng, _, err := initPolicy(path, db, nil); err == nil || eng != nil {
			t.Fatalf("invalid quorum %s accepted", value)
		}
	}
}

func writeSecurityCompletionConfig(t *testing.T, dir, raw string) string {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["graphs"] = map[string]string{"echo": "graph.py:graph"}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return writeLangGraphJSON(t, dir, string(encoded))
}
