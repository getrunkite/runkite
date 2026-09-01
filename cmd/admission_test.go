package main

import "testing"

// clearAdmissionEnv blanks every env var admissionProblems consults, via
// t.Setenv (auto-restored after the test) -- without this, a developer's
// own shell environment (e.g. POSTGRES_DSN exported for other Makefile
// targets) could make a test case pass or fail depending on what's
// ambiently set, not what the test case itself configures.
func clearAdmissionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"POSTGRES_DSN", "MYSQL_DSN", "MONGO_URI",
		"REDIS_URL", "NATS_URL", "KAFKA_URL",
		"RUNNER_TOKEN_PYTHON_LANGGRAPH", "RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH", "RUNNER_TENANTS_TYPESCRIPT_LANGGRAPHJS",
		"RUNKITE_MODE", "RUNKITE_ALLOW_INSECURE_SERVE",
	} {
		t.Setenv(name, "")
	}
}

// TestAdmissionProblems locks checkProductionAdmission requirements
// (durable state, shared transport, runner tokens, client-facing auth,
// runner-tenant allow-lists when auth+tokens are both on) and the three
// bypasses (dev mode, RUNKITE_MODE=test, RUNKITE_ALLOW_INSECURE_SERVE) --
// found by an external audit that bare `serve` silently accepted SQLite +
// open auth + trusted runners; see checkProductionAdmission's own doc
// comment for the live-verified symptom this closes.
func TestAdmissionProblems(t *testing.T) {
	noAuthConfig := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo_agent":"./graph.py:graph"}}`)
	withAuthConfig := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo_agent":"./graph.py:graph"},"auth":{"type":"api_key","keys":{"sk-test":{"name":"tester"}}}}`)
	withAdminKeysOnlyConfig := writeLangGraphJSON(t, t.TempDir(), `{"graphs":{"echo_agent":"./graph.py:graph"},"auth":{"admin_keys":{"sk-admin":"operator"}}}`)

	tests := []struct {
		name         string
		opts         serverOpts
		env          map[string]string
		wantProblems int // -1 means "don't care about count, just >0"
	}{
		{
			name:         "dev mode always bypasses regardless of config",
			opts:         serverOpts{devMode: true, configPath: noAuthConfig},
			wantProblems: 0,
		},
		{
			name:         "RUNKITE_MODE=test bypasses serve",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			env:          map[string]string{"RUNKITE_MODE": "test"},
			wantProblems: 0,
		},
		{
			name:         "RUNKITE_ALLOW_INSECURE_SERVE bypasses serve",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			env:          map[string]string{"RUNKITE_ALLOW_INSECURE_SERVE": "1"},
			wantProblems: 0,
		},
		{
			name: "fully configured serve has no problems",
			opts: serverOpts{devMode: false, configPath: withAuthConfig},
			env: map[string]string{
				"POSTGRES_DSN": "postgres://x", "REDIS_URL": "redis://x",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
				"RUNNER_TENANTS_PYTHON_LANGGRAPH": "default",
			},
			wantProblems: 0,
		},
		{
			name: "auth+tokens without RUNNER_TENANTS fail closed",
			opts: serverOpts{devMode: false, configPath: withAuthConfig},
			env: map[string]string{
				"POSTGRES_DSN": "postgres://x", "REDIS_URL": "redis://x",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok",
			},
			wantProblems: 1,
		},
		{
			name: "partial RUNNER_TENANTS across kinds fail closed",
			opts: serverOpts{devMode: false, configPath: withAuthConfig},
			env: map[string]string{
				"POSTGRES_DSN": "postgres://x", "REDIS_URL": "redis://x",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH":        "tok-py",
				"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS":  "tok-ts",
				"RUNNER_TENANTS_PYTHON_LANGGRAPH":      "default",
			},
			wantProblems: 1,
		},
		{
			name:         "bare serve with nothing configured has all four problems",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			wantProblems: 4,
		},
		{
			name:         "durable state alone still leaves transport/tokens/auth unmet",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			env:          map[string]string{"POSTGRES_DSN": "postgres://x"},
			wantProblems: 3,
		},
		{
			name:         "MySQL also counts as a durable backend",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			env:          map[string]string{"MYSQL_DSN": "root@/x", "REDIS_URL": "redis://x", "RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok"},
			wantProblems: 1, // auth still unmet (no tenant check without client auth)
		},
		{
			name: "Mongo also counts as a durable backend",
			opts: serverOpts{devMode: false, configPath: withAuthConfig},
			env: map[string]string{
				"MONGO_URI": "mongodb://x", "NATS_URL": "nats://x",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
				"RUNNER_TENANTS_PYTHON_LANGGRAPH": "default",
			},
			wantProblems: 0,
		},
		{
			name: "Kafka also counts as shared transport",
			opts: serverOpts{devMode: false, configPath: withAuthConfig},
			env: map[string]string{
				"POSTGRES_DSN": "postgres://x", "KAFKA_URL": "localhost:9092",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
				"RUNNER_TENANTS_PYTHON_LANGGRAPH": "default",
			},
			wantProblems: 0,
		},
		{
			name: "admin_keys alone (no primary auth.type) counts as client-facing auth",
			opts: serverOpts{devMode: false, configPath: withAdminKeysOnlyConfig},
			env: map[string]string{
				"POSTGRES_DSN": "postgres://x", "REDIS_URL": "redis://x",
				"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
				"RUNNER_TENANTS_PYTHON_LANGGRAPH": "default",
			},
			wantProblems: 0,
		},
		{
			name:         "backends+tokens configured but no auth still fails",
			opts:         serverOpts{devMode: false, configPath: noAuthConfig},
			env:          map[string]string{"POSTGRES_DSN": "postgres://x", "REDIS_URL": "redis://x", "RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok"},
			wantProblems: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAdmissionEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got := admissionProblems(tt.opts)
			if len(got) != tt.wantProblems {
				t.Errorf("admissionProblems() = %d problems %v, want %d", len(got), got, tt.wantProblems)
			}
		})
	}
}
