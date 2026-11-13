package reviewha_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const fixtureCredential = "synthetic-sensitive-provider-credential"
const fixtureArgument = "synthetic-sensitive-argument"
const fixtureRPCID = "synthetic-sensitive-rpc-id"
const fixtureReply = "synthetic-sensitive-tool-reply"

// These tests qualify the assembled HTTP boundary in separate serve processes.
// Run/attempt/queue assignments are synthetic fixture setup, not SDK execution or
// run-admission coverage. Only fresh disposable loopback Postgres/Redis are used.
type haSuite struct {
	t                       *testing.T
	ctx                     context.Context
	directory, output       string
	binary, config          string
	postgresURL, redisURL   string
	runnerKind, runnerToken string
	secretValues            []string
	client                  *http.Client
	store                   *postgres.Store
	redis                   *redis.Client
	queue                   *redistransport.Queue
	replicas                []*replica
	current                 [2]*replica
	assignments             []*transport.RunAssignment
	report                  map[string]any
}

type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }

type replica struct {
	name, base, httpPort, grpcPort string
	command                        *exec.Cmd
	done                           chan struct{}
	log                            lockedBuffer
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

type tenantFixture struct {
	tenant, run, thread, readKey, approveKey string
	generation                               int64
}

func loopbackURL(t *testing.T, value, scheme string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != scheme || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1") || parsed.Port() == "" {
		t.Fatal("HA qualification requires explicit disposable loopback test URLs with ports")
	}
	return parsed
}

func newHASuite(t *testing.T) *haSuite {
	t.Helper()
	postgresURL := os.Getenv("RUNKITE_TEST_REVIEW_POSTGRES_DSN")
	redisURL := os.Getenv("RUNKITE_TEST_REVIEW_REDIS_URL")
	pgURL := loopbackURL(t, postgresURL, "postgres")
	if !strings.HasPrefix(strings.TrimPrefix(pgURL.Path, "/"), "runkite_review_ha_") {
		t.Fatal("Postgres database must have the dedicated runkite_review_ha_ prefix")
	}
	loopbackURL(t, redisURL, "redis")
	binary, err := filepath.Abs(os.Getenv("RUNKITE_TEST_BINARY"))
	if err != nil || os.Getenv("RUNKITE_TEST_BINARY") == "" {
		t.Fatal("RUNKITE_TEST_BINARY must identify an actual built runkite binary")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		t.Fatal("RUNKITE_TEST_BINARY must be executable")
	}
	directory := t.TempDir()
	output := os.Getenv("RUNKITE_TEST_REVIEW_OUTPUT")
	if output == "" {
		output = directory
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &haSuite{t: t, ctx: ctx, directory: directory, output: output, binary: binary, postgresURL: postgresURL, redisURL: redisURL, runnerKind: "review-ha-" + uuid.NewString(), runnerToken: uuid.NewString(), client: &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	s.secretValues = []string{s.runnerToken, fixtureCredential, fixtureArgument, fixtureRPCID, fixtureReply}
	if pgURL.User != nil {
		if password, present := pgURL.User.Password(); present && password != "" {
			s.secretValues = append(s.secretValues, password)
		}
	}
	s.report = map[string]any{"schema_version": 1, "started_at": time.Now().UTC(), "status": "running", "topology": "two actual serve processes with shared disposable Postgres and Redis", "assignment": "synthetic stored run/attempt and shared queue assignment", "non_claims": []string{"SDK/model execution or admission", "exactly-once external side effects", "database or Redis failover", "multi-day or cloud HA", "network-isolated subprocess listeners (serve retains its unmodified CLI listener scope)"}, "cases": []any{}}
	t.Cleanup(func() {
		cancel()
		for _, p := range s.replicas {
			s.stop(p, false)
		}
		if s.queue != nil {
			for _, a := range s.assignments {
				_, _ = s.queue.Ack(context.Background(), a.RunID, a.Generation)
			}
		}
		if s.store != nil {
			s.store.Close()
		}
		if s.redis != nil {
			_ = s.redis.Close()
		}
		for i, p := range s.replicas {
			if err := os.WriteFile(filepath.Join(s.output, fmt.Sprintf("replica-%s-%02d.log", p.name, i)), []byte(s.redact(p.log.String())), 0600); err != nil {
				t.Errorf("persist sanitized replica log: %v", err)
			}
		}
		if t.Failed() {
			s.report["status"] = "fail"
		} else {
			s.report["status"] = "pass"
		}
		s.report["finished_at"] = time.Now().UTC()
		s.saveReport()
	})
	// Refuse a preexisting schema or populated Redis before any production process
	// can run maintenance against it. This harness never truncates or flushes data.
	conn, err := pgx.Connect(ctx, postgresURL)
	if err != nil {
		t.Fatal("cannot connect to disposable Postgres")
	}
	var tables int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables)
	_ = conn.Close(ctx)
	if err != nil || tables != 0 {
		t.Fatal("HA qualification requires a fresh empty Postgres database")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil || options.DB != 0 {
		t.Fatal("HA qualification requires dedicated Redis DB0")
	}
	s.redis = redis.NewClient(options)
	if size, err := s.redis.DBSize(ctx).Result(); err != nil || size != 0 {
		t.Fatal("HA qualification requires a fresh empty Redis instance")
	}
	s.store, err = postgres.New(ctx, postgresURL)
	if err != nil {
		t.Fatal("cannot open disposable Postgres state store")
	}
	if err := s.store.Init(ctx); err != nil {
		t.Fatalf("initialize disposable state: %s", s.redact(err.Error()))
	}
	s.queue = redistransport.NewQueue(s.redis)
	return s
}

func (s *haSuite) redact(value string) string {
	for _, secret := range s.secretValues {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[synthetic-value]")
		}
	}
	return value
}

func (s *haSuite) saveReport() {
	raw, err := json.MarshalIndent(s.report, "", "  ")
	if err != nil {
		s.t.Errorf("encode Review HA report: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.output, "review-ha-report.json"), append(raw, '\n'), 0600); err != nil {
		s.t.Errorf("persist Review HA report: %v", err)
	}
}

func (s *haSuite) record(name string, detail any) {
	s.t.Helper()
	s.report["cases"] = append(s.report["cases"].([]any), map[string]any{"name": name, "status": "pass", "at": time.Now().UTC(), "evidence": detail})
	s.saveReport()
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func (s *haSuite) start(index int) *replica {
	return s.startWithConfig(index, s.config)
}

func (s *haSuite) startWithConfig(index int, config string) *replica {
	s.t.Helper()
	p := &replica{name: strconv.Itoa(index + 1), httpPort: freePort(s.t), grpcPort: freePort(s.t), done: make(chan struct{})}
	p.base = "http://127.0.0.1:" + p.httpPort
	p.command = exec.Command(s.binary, "serve", "--config", config, "--port", p.httpPort, "--grpc-port", p.grpcPort)
	// An explicit environment prevents inherited providers, hooks, telemetry,
	// databases, admin credentials or developer .env settings entering the test.
	encodedKind := strings.ToUpper(strings.ReplaceAll(s.runnerKind, "-", "_"))
	allowedTenants := []string{tenant.DefaultTenant}
	for _, assignment := range s.assignments {
		allowedTenants = append(allowedTenants, assignment.TenantID)
	}
	// Synthetic assignments stay claimed throughout bounded repetitions. Runner
	// lease recovery is exercised by the existing separate recovery harness.
	p.command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "LOG_FORMAT=json", "POSTGRES_DSN=" + s.postgresURL, "REDIS_URL=" + s.redisURL, "RUNNER_TOKEN_" + encodedKind + "=" + s.runnerToken, "RUNNER_TENANTS_" + encodedKind + "=" + strings.Join(allowedTenants, ","), "RUNKITE_RECLAIM_MAX_AGE=2h"}
	p.command.Dir = s.directory
	p.command.Stdout = &p.log
	p.command.Stderr = &p.log
	if err := p.command.Start(); err != nil {
		s.t.Fatal("launch actual control plane:", err)
	}
	s.replicas = append(s.replicas, p)
	s.current[index] = p
	go func() { _ = p.command.Wait(); close(p.done) }()
	s.wait("replica readiness", 20*time.Second, func() bool {
		select {
		case <-p.done:
			s.t.Fatalf("control plane exited before readiness: %s", s.redact(p.log.String()))
		default:
		}
		return s.request(p, "GET", "/readyz", "", nil, nil, "").status == 200
	})
	return p
}

func (s *haSuite) stop(p *replica, hard bool) {
	if p == nil || p.command == nil || p.command.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	if hard {
		_ = p.command.Process.Kill()
	} else {
		_ = p.command.Process.Signal(os.Interrupt)
	}
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
		_ = p.command.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		s.t.Error("test control plane did not exit")
	}
}

func (s *haSuite) wait(description string, timeout time.Duration, check func() bool) {
	s.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-s.ctx.Done():
			s.t.Fatal("HA qualification cancelled")
		case <-deadline.C:
			s.t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func (s *haSuite) request(p *replica, method, path, key string, body any, cookie *http.Cookie, csrf string) httpResult {
	return s.do(s.client, p, method, path, key, body, cookie, csrf, nil)
}

func (s *haSuite) do(client *http.Client, p *replica, method, path, key string, body any, cookie *http.Cookie, csrf string, headers http.Header) httpResult {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(s.ctx, method, p.base+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(auth.HeaderCSRF, csrf)
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	resp, err := client.Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if len(raw) > 2<<20 {
		err = errors.New("test HTTP response exceeded bound")
	}
	return httpResult{status: resp.StatusCode, header: resp.Header.Clone(), body: raw, err: err}
}

func (s *haSuite) expect(result httpResult, status int) {
	s.t.Helper()
	if result.err != nil || result.status != status {
		s.t.Fatalf("HTTP status=%d want=%d error=%v body=%s", result.status, status, result.err, s.redact(string(result.body)))
	}
}

func decode[T any](t *testing.T, result httpResult) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.body, &value); err != nil {
		t.Fatal("invalid test response JSON")
	}
	return value
}

func (s *haSuite) seed(f tenantFixture) {
	s.t.Helper()
	ctx := tenant.ScopedContext(s.ctx, f.tenant)
	now := time.Now().UTC()
	if err := s.store.CreateThread(ctx, &models.Thread{ThreadID: f.thread, Status: models.ThreadStatusBusy, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		s.t.Fatal(err)
	}
	manifest := models.RunManifest{SchemaVersion: 1, TenantID: f.tenant, AgentID: "ha-agent", AgentVersion: 1, Principal: &models.RunManifestPrincipal{Identity: "synthetic-agent"}}
	if err := s.store.CreateRun(ctx, &models.Run{RunID: f.run, ThreadID: f.thread, AgentID: "ha-agent", Status: models.RunStatusRunning, Metadata: map[string]any{"run_manifest": manifest}, CreatedAt: now, UpdatedAt: now}); err != nil {
		s.t.Fatal(err)
	}
	if accepted, err := s.store.BeginRunAttempt(ctx, f.run, f.generation); err != nil || !accepted {
		s.t.Fatal("could not create synthetic durable attempt")
	}
	a := &transport.RunAssignment{RunID: f.run, ThreadID: f.thread, TenantID: f.tenant, GraphID: "ha-agent", Generation: f.generation, RunnerKind: s.runnerKind, User: &transport.UserContext{Identity: "synthetic-agent"}}
	if err := s.queue.Enqueue(ctx, a); err != nil {
		s.t.Fatal(err)
	}
	live, err := s.queue.Dequeue(ctx, s.runnerKind, time.Second)
	if err != nil || live == nil || live.RunID != f.run || live.Generation != f.generation {
		s.t.Fatal("could not claim synthetic shared assignment")
	}
	s.assignments = append(s.assignments, a)
}
