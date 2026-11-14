package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/execution"
	"github.com/getrunkite/runkite/internal/transport"
)

func poolFixture(t *testing.T) (*RunnerTokens, string) {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(raw[:])
	rt := &RunnerTokens{tokens: map[string][]string{"pool-finance": {token}}, tenants: map[string]map[string]struct{}{}}
	pools, err := execution.NewCatalog(map[string]execution.Pool{"finance": {RunnerKind: "python-langgraph", Tenants: []string{"acme"}, Agents: []string{"finance"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.ConfigureExecutionPools(pools); err != nil {
		t.Fatal(err)
	}
	return rt, token
}

func TestPoolCredentialsRequireDeclaredIndependentAuthority(t *testing.T) {
	rt, token := poolFixture(t)
	if !rt.Validate("pool-finance", token) || rt.Validate("pool-unknown", token) || (*RunnerTokens)(nil).Validate("pool-unknown", token) {
		t.Fatal("pool credential boundary failed")
	}
	if !rt.AllowsAssignment("pool-finance", "acme", "finance") || rt.AllowsAssignment("pool-finance", "other", "finance") || rt.AllowsAssignment("pool-finance", "acme", "other") {
		t.Fatal("pool assignment boundary failed")
	}
	rt.tokens["python-langgraph"] = []string{token}
	if err := rt.ConfigureExecutionPools(rt.pools); err == nil {
		t.Fatal("shared credential accepted")
	}
}

type poolInflight map[string]*transport.RunAssignment

func (p poolInflight) LookupInflight(_ context.Context, id string) (*transport.RunAssignment, error) {
	return p[id], nil
}

func TestPoolHTTPDoesNotExposeUnboundOrForeignData(t *testing.T) {
	rt, token := poolFixture(t)
	assignments := poolInflight{"one": {RunID: "one", RunnerKind: "pool-finance", TenantID: "acme", GraphID: "finance", Generation: 1}, "foreign": {RunID: "foreign", RunnerKind: "pool-finance", TenantID: "acme", GraphID: "other", Generation: 1}}
	for _, tc := range []struct {
		method, path, run string
		status            int
	}{
		{"GET", "/internal/cron", "", 401},
		{"GET", "/internal/runs/one/status", "", 401},
		{"GET", "/internal/runs/one/status", "one", 200},
		{"GET", "/internal/runs/foreign/status", "one", 403},
		{"GET", "/internal/store/items", "foreign", 403},
		{"GET", "/internal/store/items", "one", 200},
		{"GET", "/internal/connectors", "one", 403},
		{"PUT", "/internal/agents/finance/schema", "", 200},
		{"PUT", "/internal/agents/other/schema", "", 401},
	} {
		t.Run(tc.method+tc.path+tc.run, func(t *testing.T) {
			calls := 0
			h := MiddlewareWithOpts(nil, nil, rt, MiddlewareOpts{Inflight: assignments}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set(HeaderRunnerKind, "pool-finance")
			r.Header.Set(HeaderRunnerToken, token)
			r.Header.Set(HeaderTenantID, "acme")
			if tc.run != "" {
				r.Header.Set(HeaderRunID, tc.run)
				r.Header.Set(HeaderGeneration, "1")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || (tc.status != 200 && calls != 0) {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}
