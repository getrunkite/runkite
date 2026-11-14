package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"

	"github.com/getrunkite/runkite/internal/credentials"
)

type liveFixture struct {
	address, tokenFile, dsn string
	provider                *credentials.VaultProvider
	store                   *credentials.SQLStore
}

func fixtureSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func liveVaultPostgres(t *testing.T) liveFixture {
	t.Helper()
	if os.Getenv("RUNKITE_TEST_VAULT_DATABASE") != "1" {
		t.Skip("set RUNKITE_TEST_VAULT_DATABASE=1 for owned local Vault and PostgreSQL qualification")
	}
	if _, err := exec.LookPath("vault"); err != nil {
		t.Fatal("local Vault CLI required")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("Docker required")
	}
	tmp := t.TempDir()
	password := fixtureSecret(t)
	name := "runkite-vault-db-" + uuid.NewString()
	envFile := filepath.Join(tmp, "postgres.env")
	if err := os.WriteFile(envFile, []byte("POSTGRES_PASSWORD="+password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("RUNKITE_TEST_POSTGRES_IMAGE")
	if image == "" {
		image = "postgres:17-alpine"
	}
	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-d", "--name", name, "--env-file", envFile, "-p", "127.0.0.1::5432", image)
	if _, err := cmd.Output(); err != nil {
		t.Fatal("could not start owned PostgreSQL fixture")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	})
	portOutput, err := exec.CommandContext(t.Context(), "docker", "inspect", "--format", `{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}`, name).Output()
	if err != nil {
		t.Fatal("could not discover owned PostgreSQL fixture port")
	}
	port := strings.TrimSpace(string(portOutput))
	dsn := fmt.Sprintf("host=127.0.0.1 port=%s dbname=postgres sslmode=disable", port)
	adminCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture PostgreSQL target")
	}
	adminCfg.User = "postgres"
	adminCfg.Password = password
	var admin *pgx.Conn
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		admin, err = pgx.ConnectConfig(t.Context(), adminCfg)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("owned PostgreSQL fixture did not become ready")
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(t.Context(), `CREATE TABLE public.runkite_reports (id integer PRIMARY KEY); INSERT INTO public.runkite_reports VALUES (1),(2),(3)`); err != nil {
		t.Fatal("could not seed synthetic fixture rows")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + listener.Addr().String()
	_ = listener.Close()
	rootToken := fixtureSecret(t)
	logFile, err := os.OpenFile(filepath.Join(tmp, "vault-private.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	vaultConfig := filepath.Join(tmp, "vault-fixture.hcl")
	if err := os.WriteFile(vaultConfig, []byte("disable_mlock = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	vault := exec.Command("vault", "server", "-dev", "-dev-no-store-token", "-config="+vaultConfig, "-dev-root-token-id="+rootToken, "-dev-listen-address="+strings.TrimPrefix(addr, "http://"))
	// Do not inherit a developer's Vault, cloud or professional environment.
	// The explicit config and dev-no-store-token flag avoid token-helper writes.
	vault.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmp, "VAULT_ADDR=" + addr, "VAULT_API_ADDR=" + addr, "TZ=UTC"}
	vault.Stdout = logFile
	vault.Stderr = logFile
	if err := vault.Start(); err != nil {
		t.Fatal("could not start owned Vault fixture")
	}
	exited := make(chan error, 1)
	go func() { exited <- vault.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = vault.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		_ = logFile.Close()
	})
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline = time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, e := client.Get(addr + "/v1/sys/health")
		if e == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		select {
		case <-exited:
			data, _ := os.ReadFile(filepath.Join(tmp, "vault-private.log"))
			code := "process_exited"
			for _, line := range strings.Split(string(data), "\n") {
				lower := strings.ToLower(line)
				if !strings.Contains(lower, "error") && !strings.Contains(lower, "failed") {
					continue
				}
				line = strings.ReplaceAll(strings.ReplaceAll(line, rootToken, "[fixture token]"), password, "[fixture password]")
				if len(line) > 400 {
					line = line[:400]
				}
				code = line
				break
			}
			t.Fatalf("Vault fixture GET sys/health unavailable: %s", code)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Vault fixture GET sys/health unavailable: readiness_deadline")
	}
	write := func(path string, payload any) map[string]any {
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, addr+"/v1/"+path, bytes.NewReader(b))
		req.Header.Set("X-Vault-Token", rootToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			code := "request_failed"
			if errors.Is(err, context.DeadlineExceeded) {
				code = "deadline"
			} else if errors.Is(err, syscall.ECONNREFUSED) {
				code = "connection_refused"
			}
			t.Fatalf("Vault fixture POST %s unavailable: %s", path, code)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		if resp.StatusCode != 200 && resp.StatusCode != 204 {
			t.Fatalf("Vault fixture configuration rejected for %s: HTTP %d", path, resp.StatusCode)
		}
		var result map[string]any
		if len(data) > 0 && json.Unmarshal(data, &result) != nil {
			t.Fatal("invalid Vault fixture configuration response")
		}
		return result
	}
	write("sys/mounts/database", map[string]any{"type": "database"})
	write("database/config/reporting", map[string]any{"plugin_name": "postgresql-database-plugin", "allowed_roles": []string{"readonly"}, "connection_url": fmt.Sprintf("postgresql://{{username}}:{{password}}@127.0.0.1:%s/postgres?sslmode=disable", port), "username": "postgres", "password": password})
	write("database/roles/readonly", map[string]any{"db_name": "reporting", "default_ttl": "30s", "max_ttl": "60s", "creation_statements": []string{`CREATE ROLE "{{name}}" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}'; GRANT CONNECT ON DATABASE postgres TO "{{name}}"; GRANT USAGE ON SCHEMA public TO "{{name}}"; GRANT SELECT ON public.runkite_reports TO "{{name}}";`}, "revocation_statements": []string{`REVOKE ALL PRIVILEGES ON public.runkite_reports FROM "{{name}}"; REVOKE USAGE ON SCHEMA public FROM "{{name}}"; REVOKE CONNECT ON DATABASE postgres FROM "{{name}}"; DROP ROLE IF EXISTS "{{name}}";`}})
	policy := `path "database/creds/readonly" { capabilities = ["read"] }
path "sys/leases/lookup" {
  capabilities = ["update"]
  allowed_parameters = { "lease_id" = ["database/creds/readonly/*"] }
}
path "sys/leases/renew" {
  capabilities = ["update"]
  allowed_parameters = {
    "lease_id" = ["database/creds/readonly/*"]
    "increment" = []
  }
}
path "sys/leases/revoke" {
  capabilities = ["update"]
  allowed_parameters = {
    "lease_id" = ["database/creds/readonly/*"]
    "sync" = [true]
  }
}
`
	write("sys/policies/acl/runkite-fixture", map[string]any{"policy": policy})
	created := write("auth/token/create", map[string]any{"policies": []string{"runkite-fixture"}, "no_default_policy": true, "ttl": "1h"})
	auth, ok := created["auth"].(map[string]any)
	if !ok {
		t.Fatal("workload token not returned")
	}
	token, ok := auth["client_token"].(string)
	if !ok || token == "" {
		t.Fatal("workload token not returned")
	}
	tokenFile := filepath.Join(tmp, "vault-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := credentials.NewVaultProvider(credentials.VaultConfig{Address: addr, TokenFile: tokenFile, Mount: "database", Role: "readonly", AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.User = "postgres"
	poolCfg.ConnConfig.Password = password
	pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
	if err != nil {
		t.Fatal("could not create owned fixture ledger")
	}
	t.Cleanup(pool.Close)
	stmts, err := credentials.Schema(credentials.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(t.Context(), stmt); err != nil {
			t.Fatal("could not create credential ledger schema")
		}
	}
	store, err := credentials.NewPGXStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return liveFixture{addr, tokenFile, dsn, p, store}
}

func liveManager(t *testing.T, p credentials.Provider, revision string, provided ...*credentials.SQLStore) (*credentials.Manager, *credentials.SQLStore) {
	t.Helper()
	var store *credentials.SQLStore
	if len(provided) > 0 {
		store = provided[0]
	} else {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(1)
		stmts, _ := credentials.Schema(credentials.SQLite)
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		store, err = credentials.NewSQLStore(db, credentials.SQLite)
		if err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyring, err := credentials.NewKeyring("fixture", map[string][]byte{"fixture": key})
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	m, err := credentials.NewManager(store, keyring, []credentials.Binding{{ID: "reporting", Revision: revision, ConsumerID: "reporting-mcp", MaxTTL: time.Minute, RenewIncrement: 15 * time.Second, Provider: p}}, func(context.Context, credentials.Owner, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func liveOwner(action string) credentials.Owner {
	return credentials.Owner{TenantID: "synthetic-tenant", RunID: "synthetic-run", RunIncarnationID: "fixture-incarnation", Generation: 1, ActionID: action, ConsumerID: "reporting-mcp"}
}

func TestLiveVaultDatabaseMCPConsumer(t *testing.T) {
	f := liveVaultPostgres(t)
	m, store := liveManager(t, f.provider, f.provider.Revision("reporting-mcp", time.Minute, 15*time.Second), f.store)
	handler, err := newExecutor(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	var username, password string
	r, err := m.Execute(t.Context(), liveOwner("allowed-action"), "reporting", func(ctx context.Context, u credentials.UserPassword) error {
		username, password = u.Values()
		cfg, _ := pgx.ParseConfig(f.dsn)
		cfg.User = username
		cfg.Password = password
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal("issued credential could not authenticate")
		}
		if _, err := conn.Exec(ctx, `INSERT INTO public.runkite_reports VALUES (4)`); err == nil {
			t.Fatal("Vault readonly role allowed a write")
		}
		_ = conn.Close(context.Background())
		rows, err := store.Search(ctx, "synthetic-tenant", 10)
		if err != nil || len(rows) != 1 || rows[0].GrantedTTLSeconds != 30 {
			t.Fatal("actual provider TTL not recorded")
		}
		renewed, err := m.Renew(ctx, liveOwner("allowed-action"), rows[0].ID)
		if err != nil || renewed.GrantedTTLSeconds != 15 {
			t.Fatal("provider renewal failed or requested TTL was misrepresented", err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"report_count","arguments":{}}}`))
		req.SetBasicAuth(username, password)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "report_count=3") || strings.Contains(string(body), username) || strings.Contains(string(body), password) {
			t.Fatal("external MCP did not return a secret-free real read result")
		}
		return nil
	})
	if err != nil || r.State != credentials.StateRevoked {
		t.Fatal("provider-confirmed cleanup failed", err, r)
	}
	cfg, _ := pgx.ParseConfig(f.dsn)
	cfg.User = username
	cfg.Password = password
	if conn, err := pgx.ConnectConfig(t.Context(), cfg); err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("revoked credential still authenticates to PostgreSQL")
	}
	if _, err := m.Execute(t.Context(), liveOwner("allowed-action"), "reporting", func(context.Context, credentials.UserPassword) error { t.Fatal("replayed consumer ran"); return nil }); !errors.Is(err, credentials.ErrAlreadyIssued) {
		t.Fatal(err)
	}
}

type lostIssueResponse struct {
	credentials.Provider
	captured credentials.Grant
}

func (p *lostIssueResponse) Issue(ctx context.Context) (credentials.Grant, error) {
	g, err := p.Provider.Issue(ctx)
	if err != nil {
		return g, err
	}
	p.captured = g
	return credentials.Grant{}, errors.New("simulated lost issuance acknowledgement")
}

type lostRevokeResponse struct {
	credentials.Provider
	lost bool
}

func (p *lostRevokeResponse) Revoke(ctx context.Context, handle string) error {
	err := p.Provider.Revoke(ctx, handle)
	if err != nil {
		return err
	}
	if !p.lost {
		p.lost = true
		return errors.New("simulated lost revoke acknowledgement")
	}
	return nil
}

func TestLiveVaultUnknownIssuanceAndRevocation(t *testing.T) {
	f := liveVaultPostgres(t)
	t.Run("lost issuance is not retried", func(t *testing.T) {
		p := &lostIssueResponse{Provider: f.provider}
		m, _ := liveManager(t, p, "lost-issue-fixture", f.store)
		r, err := m.Execute(t.Context(), liveOwner("lost-issuance"), "reporting", func(context.Context, credentials.UserPassword) error { t.Fatal("unknown credential used"); return nil })
		if !errors.Is(err, credentials.ErrIssuanceUnknown) || r.State != credentials.StateIssuanceUnknown || p.captured.TTL <= 0 || p.captured.TTL > time.Minute {
			t.Fatal("actual lost issuance not represented correctly")
		}
		if status, err := f.provider.Lookup(t.Context(), p.captured.Handle); err != nil || !status.Found {
			t.Fatal("lost response did not leave a real live provider lease")
		}
		if _, err := m.Reconcile(t.Context(), 100); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Execute(t.Context(), liveOwner("lost-issuance"), "reporting", func(context.Context, credentials.UserPassword) error { return nil }); !errors.Is(err, credentials.ErrAlreadyIssued) {
			t.Fatal("unknown issuance was reissued")
		}
		// Only the test's fault injector retained this handle. The production
		// ledger cannot rediscover it or claim confirmed cleanup without it.
		if err := f.provider.Revoke(t.Context(), p.captured.Handle); err != nil {
			t.Fatal("fixture orphan cleanup failed")
		}
	})
	t.Run("lost revoke is reconciled by provider lookup", func(t *testing.T) {
		p := &lostRevokeResponse{Provider: f.provider}
		m, _ := liveManager(t, p, "lost-revoke-fixture", f.store)
		r, err := m.Execute(t.Context(), liveOwner("lost-revocation"), "reporting", func(context.Context, credentials.UserPassword) error { return nil })
		if !errors.Is(err, credentials.ErrCleanupPending) || r.State != credentials.StateRevocationUnknown {
			t.Fatal("lost revoke was falsely confirmed")
		}
		if _, err := m.Reconcile(t.Context(), 100); err != nil {
			t.Fatal("actual Vault absence was not reconciled", err)
		}
		actual, err := m.Get(t.Context(), r.Owner.TenantID, r.ID)
		if err != nil || actual.State != credentials.StateRevoked || actual.ReasonCode != "provider_confirmed_absent" {
			t.Fatal("reconciled absence missing", err)
		}
	})
}
