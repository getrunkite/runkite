package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	goredis "github.com/redis/go-redis/v9"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/sharanharsoor/runkite/internal/api"
	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/bridge"
	pb "github.com/sharanharsoor/runkite/internal/bridge/runnerpb"
	"github.com/sharanharsoor/runkite/internal/config"
	"github.com/sharanharsoor/runkite/internal/connector"
	"github.com/sharanharsoor/runkite/internal/cors"
	"github.com/sharanharsoor/runkite/internal/hooks"
	"github.com/sharanharsoor/runkite/internal/metrics"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/ratelimit"
	"github.com/sharanharsoor/runkite/internal/state"
	mongostore "github.com/sharanharsoor/runkite/internal/state/mongo"
	mysqlstore "github.com/sharanharsoor/runkite/internal/state/mysql"
	pgstore "github.com/sharanharsoor/runkite/internal/state/postgres"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/tracing"
	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
	redistransport "github.com/sharanharsoor/runkite/internal/transport/redis"
	"github.com/sharanharsoor/runkite/internal/vectorstore"
	pgvector "github.com/sharanharsoor/runkite/internal/vectorstore/pgvector"
	qdrant "github.com/sharanharsoor/runkite/internal/vectorstore/qdrant"
)

func cmdDev(args []string) {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	port := fs.String("port", "", "HTTP port (default: $HTTP_PORT or 2026)")
	grpcPort := fs.String("grpc-port", "", "gRPC port (default: $GRPC_PORT or 50051)")
	configPath := fs.String("config", "", "path to langgraph.json (default: auto-discover)")
	fs.StringVar(port, "p", "", "HTTP port (shorthand)")
	fs.StringVar(configPath, "c", "", "path to langgraph.json (shorthand)")
	fs.Parse(args)

	opts := serverOpts{
		httpPort:   resolvePort(*port, "HTTP_PORT", "2026"),
		grpcPort:   resolvePort(*grpcPort, "GRPC_PORT", "50051"),
		configPath: resolveConfig(*configPath),
		devMode:    true,
	}
	startServer(opts)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "", "HTTP port (default: $HTTP_PORT or 2026)")
	grpcPort := fs.String("grpc-port", "", "gRPC port (default: $GRPC_PORT or 50051)")
	configPath := fs.String("config", "", "path to langgraph.json (required or set LANGGRAPH_CONFIG)")
	fs.StringVar(port, "p", "", "HTTP port (shorthand)")
	fs.StringVar(configPath, "c", "", "path to langgraph.json (shorthand)")
	fs.Parse(args)

	opts := serverOpts{
		httpPort:   resolvePort(*port, "HTTP_PORT", "2026"),
		grpcPort:   resolvePort(*grpcPort, "GRPC_PORT", "50051"),
		configPath: resolveConfig(*configPath),
		devMode:    false,
	}
	startServer(opts)
}

type serverOpts struct {
	httpPort   string
	grpcPort   string
	configPath string
	devMode    bool
}

func startServer(opts serverOpts) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx := context.Background()

	// OTel (master plan: "OTel observability fan-out"). No-op, zero
	// overhead, and no background connection attempts unless
	// OTEL_EXPORTER_OTLP_ENDPOINT is set -- see internal/tracing.
	shutdownTracing, err := tracing.Init(ctx)
	if err != nil {
		slog.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	// `defer shutdownTracing(ctx)` alone is not enough: http.ListenAndServe
	// below blocks forever and this function only returns on error (which
	// itself calls os.Exit -- skipping every defer). In practice this
	// process is stopped via SIGTERM/SIGINT (Ctrl+C, `docker stop`, k8s pod
	// termination), which by default kills the process without running any
	// deferred functions at all. Without an explicit signal handler, the
	// OTel batch span processor's buffered-but-not-yet-exported spans
	// (default 5s export interval) are silently dropped on every single
	// restart -- confirmed empirically: spans exported fine when the
	// interval happened to elapse before a manual kill, and vanished
	// when it didn't. Shutdown() forces a final flush before exiting.
	// --- State store ---
	store := initStore(ctx)
	defer store.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down, flushing telemetry and closing store")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			slog.Error("tracing shutdown error", "error", err)
		}
		_ = store.Close()
		os.Exit(0)
	}()

	// --- Transport layer ---
	queue, broker, cancelBus := initTransport(ctx)

	// runkite_queue_depth is a gauge, not something incremented at call
	// sites -- it must be polled periodically to reflect current state.
	go pollQueueDepth(ctx, queue)

	// Reclaim jobs dequeued by a runner that then died before Ack
	// (zombie GetJob long-poll / crash between Dequeue and first event).
	go reclaimStaleJobs(ctx, queue)

	// Bootstrap agents and connectors from langgraph.json files
	bootstrapAgents(store, opts.configPath)

	// HTTP API server
	apiServer := api.NewServer(store, queue, broker, cancelBus)

	// Wire connector registry if configured
	if reg := bootstrapConnectors(opts.configPath); reg != nil {
		apiServer.SetConnectorRegistry(reg)
		slog.Info("connector registry loaded", "connectors", reg.List())
	}

	// Vector store (master plan: "Vector/semantic store"). Disabled
	// entirely unless a vector_store section is present -- never
	// implicitly enabled just because POSTGRES_DSN is set, since an
	// existing Postgres deployment may not have the pgvector extension
	// installed or permitted.
	if vs := initVectorStore(ctx, opts.configPath); vs != nil {
		apiServer.SetVectorStore(vs)
		// initVectorStore already logs the specific backend/type/config
		// it selected -- no separate log line needed here.
	}

	// Rate limiting (master plan: "per-user, per-agent, per-tenant,
	// configurable via config"). Disabled (zero overhead) unless a
	// rate_limit section is present.
	rateLimiter := initRateLimiter(opts.configPath)
	apiServer.SetRateLimiter(rateLimiter)
	if rateLimiter.Enabled() {
		slog.Info("rate limiting: enabled")
	}

	// Agent-to-Agent (A2A) delegation depth limit (master plan:
	// "Agent-to-agent (A2A)... recursion limits"). Always available at
	// POST /internal/a2a/runs; this only tunes how deep a chain may go.
	if maxDepth := initA2AMaxDepth(opts.configPath); maxDepth > 0 {
		apiServer.SetA2AMaxDepth(maxDepth)
		slog.Info("a2a: max_depth configured", "max_depth", maxDepth)
	}

	// A/B deployment routing (master plan: "Full agent versioning...
	// A/B deployment routing"). Disabled (pure pass-through) unless
	// "agent_aliases" is configured.
	if aliasCfg := initAgentAliases(opts.configPath); len(aliasCfg) > 0 {
		apiServer.SetAliasResolver(api.NewAliasResolver(aliasCfg))
		slog.Info("agent aliases: configured", "count", len(aliasCfg))
	}

	// Event hooks + webhook delivery (master plan: on_run_start,
	// on_run_complete, on_tool_call, on_error, on_interrupt).
	hookDispatcher := initHooks(opts.configPath, store)
	apiServer.SetHookDispatcher(hookDispatcher)
	if hookDispatcher.HasSinks() {
		slog.Info("event hooks: enabled")
	}

	// Custom routes (master plan: in-runner + sidecar modes -- both are a
	// reverse proxy from here, see internal/api's doc comment).
	if proxy, url := initCustomRoutesProxy(opts.configPath); proxy != nil {
		apiServer.SetCustomRoutesProxy(proxy)
		slog.Info("custom routes: enabled", "proxy_url", url, "mount", "/custom/*")
	}

	// Cron scheduler (master plan: "cron-expression scheduling with
	// multi-instance-safe claiming (Postgres claim window), timezone
	// support"). Schedules are bootstrapped the same way agents are (every
	// discovered langgraph.json, not just the first); the scheduler loop
	// itself runs regardless of whether any are configured -- an empty
	// ListCronSchedules result is just a no-op poll every 15s.
	bootstrapCronSchedules(store, opts.configPath)
	go runCronScheduler(ctx, store, apiServer)

	// Retention (real gap found reviewing the Admin UI: runs and
	// checkpoints grew unbounded forever, with no automatic cleanup --
	// see internal/state/store.go's PruneRuns/PruneCheckpoints doc
	// comments). Opt-in, same disabled-by-default convention as every
	// other platform extension: absent "retention" config starts no
	// background loop at all.
	if retentionCfg := initRetentionConfig(opts.configPath); retentionCfg != nil {
		slog.Info("retention: enabled", "runs_max_age", retentionCfg.runsMaxAge, "checkpoints_keep_last", retentionCfg.checkpointsKeepLast, "cron_claims_max_age", retentionCfg.cronClaimsMaxAge, "interval", retentionCfg.interval)
		go runRetentionLoop(ctx, store, retentionCfg)
	}

	// Store item TTL sweep -- unlike the opt-in retention loop above,
	// this always runs: it only deletes items already past a TTL the
	// caller explicitly requested via store.put(..., ttl=...), which are
	// already unconditionally excluded from every GetItem/SearchItems
	// read regardless of whether this loop has physically deleted them
	// yet. Not a configurable policy choice like run/checkpoint
	// retention, just hygiene.
	go runStoreTTLSweep(ctx, store)

	// gRPC bridge server
	bridgeServer := bridge.NewServer(queue, broker, cancelBus, apiServer.StatusCallback())

	// Runner auth (master plan's "two-tier" model): local mode (no
	// RUNNER_TOKEN_* env vars) trusts runners implicitly, zero setup.
	// Production mode requires a valid token per runner_kind on every gRPC
	// call and on every /internal/* HTTP request. Distinct trust boundary
	// from the client-facing auth provider above -- that one never covers
	// /internal/* at all.
	runnerTokens := auth.LoadRunnerTokensFromEnv()
	if runnerTokens.Enabled() {
		slog.Info("runner auth: enabled (production mode)")
	} else {
		slog.Info("runner auth: disabled (local mode -- runners trusted implicitly)")
	}

	// Start gRPC server
	lis, err := net.Listen("tcp", ":"+opts.grpcPort)
	if err != nil {
		slog.Error("failed to listen for gRPC", "port", opts.grpcPort, "error", err)
		os.Exit(1)
	}

	// Keepalive matters specifically because of GetJob's long-poll pattern:
	// a unary call can legitimately block server-side inside Dequeue for up
	// to 30s waiting for a job. Without keepalive, a dead client (crashed
	// runner) isn't detected until that same handler happens to notice --
	// which it may never do, since Dequeue succeeding doesn't touch the
	// client connection at all. Confirmed empirically: killing a runner
	// mid-long-poll let its zombie GetJob call win the next job's Redis
	// BRPOP ~20+ seconds later and silently lose it (the response could
	// never reach the dead client). A 2s ping interval bounds that window
	// to a few seconds instead of the full long-poll timeout -- tight
	// enough to matter for a real interrupt-restart-resume cycle, which
	// can complete in well under 5s. Must match the client's
	// grpc.keepalive_time_ms in worker.py.
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Second,
			Timeout: 2 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             1 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(bridge.UnaryAuthInterceptor(runnerTokens)),
		grpc.ChainStreamInterceptor(bridge.StreamAuthInterceptor(runnerTokens)),
	)
	pb.RegisterRunnerServiceServer(grpcServer, bridgeServer)

	go func() {
		slog.Info("gRPC bridge listening", "port", opts.grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Auth middleware wraps the API handler; /metrics is outside auth.
	// ratelimit.Middleware sits inside auth (not outside) so it can read
	// the identity auth.Middleware just set in the request context for
	// per-user limiting -- global/per-user only; per-agent limiting happens
	// separately inside createRun, see internal/ratelimit's doc comment.
	authProvider := initAuthProvider(opts.configPath)
	adminAuthProvider := initAdminAuthProvider(opts.configPath)
	rateLimited := ratelimit.Middleware(rateLimiter, apiServer.Handler())
	authedAPI := auth.Middleware(authProvider, adminAuthProvider, runnerTokens, rateLimited)

	// Top-level mux: /metrics (no auth) + everything else (auth'd API).
	// Register both /metrics and /metrics/: the catch-all "/" below would
	// otherwise swallow "/metrics/" and send it through auth (401 JSON)
	// instead of Prometheus -- same pre-router path-shape trap as /admin.
	root := http.NewServeMux()
	metricsHandler := promhttp.Handler()
	root.Handle("GET /metrics", metricsHandler)
	root.Handle("GET /metrics/", metricsHandler)
	// pprof (bench-setup: internal profiling infra, opt-in via
	// RUNKITE_PPROF=1). Off by default -- these endpoints let any caller
	// dump goroutine stacks/heap contents and, via /debug/pprof/profile,
	// force real CPU load for 30s, both a real information-disclosure
	// and DoS surface if left open in production. Registered outside
	// auth (like /metrics) so a profiling run isn't accidentally gated
	// by whatever client-facing auth provider is configured -- this is
	// an operator/developer tool, not part of the Agent Protocol surface.
	if os.Getenv("RUNKITE_PPROF") == "1" {
		mountPprof(root)
		// Both default to 0 (no samples collected at all) unless set --
		// /debug/pprof/mutex and /debug/pprof/block would otherwise
		// always come back empty regardless of real contention, silently
		// implying "no contention found" for a profile that was never
		// actually sampling anything. Rate 1 (sample every contention/
		// blocking event) is the finest granularity and the right choice
		// for a short, opt-in diagnostic run -- this flag already carries
		// its own "real DoS/info-disclosure surface" warning above, so
		// the added overhead of full sampling isn't a new class of risk.
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		slog.Info("pprof: enabled at /debug/pprof/ (RUNKITE_PPROF=1)", "mutex_block_profiling", true)
	}
	root.Handle("/", authedAPI)

	// CORS (master plan gap: a browser-based frontend on a different
	// origin can't reach the control plane at all without this -- the
	// browser blocks it client-side before auth or any handler ever
	// runs). Wraps the whole root, outside auth, so an OPTIONS preflight
	// (which carries no Authorization header by design) is answered
	// directly instead of being rejected by auth.Middleware.
	corsCfg := initCorsConfig(opts.configPath)
	corsed := cors.Middleware(corsCfg, root)
	if corsCfg.Enabled() {
		slog.Info("cors: enabled", "allow_origins", corsCfg.AllowOrigins)
	}

	// Metrics middleware wraps the entire root (including /metrics routing).
	// otelhttp is outermost so every request gets a trace context (and
	// otelhttp uses httpsnoop internally, which dynamically preserves
	// whatever optional interfaces -- Flusher for SSE, Hijacker for
	// WebSocket -- the writer beneath it implements, so it can't reintroduce
	// the SSE-breaking bug metrics.responseWriter had before its own
	// Flush()/Unwrap() fix).
	handler := metrics.HTTPMiddleware(corsed)
	handler = otelhttp.NewHandler(handler, "runkite-http")

	// Start HTTP server
	// hostname is logged (not a new config knob) specifically so multiple
	// replicas behind a load balancer are distinguishable in shared logs --
	// Docker/K8s already give each container/pod a unique hostname, so this
	// is free instance identification with zero new configuration surface.
	hostname, _ := os.Hostname()
	slog.Info("control plane starting", "http_port", opts.httpPort, "grpc_port", opts.grpcPort, "hostname", hostname)

	if opts.devMode {
		fmt.Printf("\n  Runkite Control Plane (dev)\n")
	} else {
		fmt.Printf("\n  Runkite Control Plane\n")
	}
	fmt.Printf("  HTTP API:    http://localhost:%s\n", opts.httpPort)
	fmt.Printf("  gRPC bridge: localhost:%s\n", opts.grpcPort)
	fmt.Printf("  Admin UI:    http://localhost:%s/admin/\n", opts.httpPort)
	fmt.Printf("  Health:      http://localhost:%s/health\n", opts.httpPort)
	fmt.Printf("  Metrics:     http://localhost:%s/metrics\n\n", opts.httpPort)

	if err := http.ListenAndServe(":"+opts.httpPort, handler); err != nil {
		slog.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}

// pollQueueDepth periodically samples the job queue length into the
// runkite_queue_depth gauge. Unlike counters (incremented at call sites),
// a gauge reflecting external state has to be actively polled.
//
// Kept off the request path on purpose: Redis Queue.Len is SCAN-based and
// proportional to total keyspace size. It used to run on every enqueue
// and every GetJob; under load that dominated Redis command time. A
// once-per-5s sample is enough for a human-facing gauge.
func pollQueueDepth(ctx context.Context, queue transport.JobQueue) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := queue.Len(ctx)
			if err != nil {
				slog.Warn("failed to poll queue depth", "error", err)
				continue
			}
			metrics.SetQueueDepth(n)
		}
	}
}

// runStoreTTLSweep periodically deletes store_items rows past their TTL
// expiry (see state.Store's PruneExpiredStoreItems doc comment). Runs
// once immediately on startup (same reasoning as the retention loop:
// don't wait a full interval to clean up items that may have already
// been sitting expired since before this process started), then on a
// fixed interval regardless of any "retention" config.
func runStoreTTLSweep(ctx context.Context, store state.Store) {
	const interval = 5 * time.Minute
	sweep := func() {
		n, err := store.PruneExpiredStoreItems(tenant.SystemContext(ctx))
		if err != nil {
			slog.Warn("store TTL sweep failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("store TTL sweep", "items_deleted", n)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// staleReclaimer is implemented by both in-process and Redis queues.
type staleReclaimer interface {
	ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error)
}

func reclaimStaleJobs(ctx context.Context, queue transport.JobQueue) {
	r, ok := queue.(staleReclaimer)
	if !ok {
		return
	}
	// Keepalive detects a dead runner in ~4s; reclaim shortly after so a
	// resume-after-crash can recover within a normal client retry window.
	const maxAge = 6 * time.Second
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.ReclaimStale(ctx, maxAge)
			if err != nil {
				slog.Warn("failed to reclaim stale jobs", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("reclaimed stale jobs", "count", n, "max_age", maxAge)
			}
		}
	}
}

// --- Shared helpers ---

func initStore(ctx context.Context) state.Store {
	postgresDSN := os.Getenv("POSTGRES_DSN")
	if postgresDSN != "" {
		pg, err := pgstore.New(ctx, postgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres", "error", err)
			os.Exit(1)
		}
		if err := pg.Init(ctx); err != nil {
			slog.Error("failed to initialize postgres store", "error", err)
			os.Exit(1)
		}
		slog.Info("state store: postgres")
		return pg
	}

	// MySQL: the second SQL exemplar alongside Postgres/SQLite (master
	// plan: "MySQL stays 'future SQL twin if someone needs it'" -- see
	// internal/state/mysql's package doc). Checked after POSTGRES_DSN,
	// same precedence convention as MONGO_URI below, so setting
	// multiple backend env vars at once is deterministic, not a race.
	if mysqlDSN := os.Getenv("MYSQL_DSN"); mysqlDSN != "" {
		my, err := mysqlstore.New(ctx, mysqlDSN)
		if err != nil {
			slog.Error("failed to connect to mysql", "error", err)
			os.Exit(1)
		}
		if err := my.Init(ctx); err != nil {
			slog.Error("failed to initialize mysql store", "error", err)
			os.Exit(1)
		}
		slog.Info("state store: mysql")
		return my
	}

	// MongoDB: the project's non-SQL exemplar backend (master plan:
	// proof state.Store is genuinely implementable against a document
	// store, not just SQL -- see internal/state/mongo's package doc).
	if mongoURI := os.Getenv("MONGO_URI"); mongoURI != "" {
		dbName := envOrDefault("MONGO_DB", "runkite")
		mg, err := mongostore.New(ctx, mongoURI, dbName)
		if err != nil {
			slog.Error("failed to connect to mongodb", "error", err)
			os.Exit(1)
		}
		if err := mg.Init(ctx); err != nil {
			slog.Error("failed to initialize mongodb store", "error", err)
			os.Exit(1)
		}
		slog.Info("state store: mongodb", "db", dbName)
		return mg
	}

	dbPath := envOrDefault("DATABASE_PATH", "./runkite.db")
	if dbPath == ":memory:" || envOrDefault("RUNKITE_MODE", "") == "test" {
		dbPath = ""
	}
	sq, err := sqlitestore.New(dbPath)
	if err != nil {
		slog.Error("failed to create sqlite store", "error", err)
		os.Exit(1)
	}
	if err := sq.Init(ctx); err != nil {
		slog.Error("failed to initialize sqlite store", "error", err)
		os.Exit(1)
	}
	slog.Info("state store: sqlite", "path", dbPath)
	return sq
}

func initTransport(ctx context.Context) (transport.JobQueue, transport.EventBroker, transport.CancelBroker) {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL != "" {
		opts, err := goredis.ParseURL(redisURL)
		if err != nil {
			slog.Error("failed to parse REDIS_URL", "error", err)
			os.Exit(1)
		}
		rdb := goredis.NewClient(opts)
		if err := rdb.Ping(ctx).Err(); err != nil {
			slog.Error("failed to connect to redis", "error", err)
			os.Exit(1)
		}
		queue := redistransport.NewQueue(rdb)
		broker := redistransport.NewBroker(rdb)
		cancelBus := redistransport.NewCancelBus(rdb)
		slog.Info("transport: redis", "url", redisURL)
		return queue, broker, cancelBus
	}

	slog.Info("transport: in-memory")
	return inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()
}

func bootstrapAgents(store state.Store, configPath string) {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		slog.Info("no langgraph.json found, skipping agent bootstrap")
		return
	}

	ctx := context.Background()
	for _, path := range paths {
		cfg, err := config.LoadLangGraphJSON(path)
		if err != nil {
			slog.Warn("skipping config", "path", path, "error", err)
			continue
		}
		entries, err := cfg.ParseGraphEntries()
		if err != nil {
			slog.Warn("skipping config", "path", path, "error", err)
			continue
		}
		for _, entry := range entries {
			metadata := map[string]interface{}{"source": path, "symbol": entry.Symbol, "runner_kind": cfg.RunnerKind}
			if needs, ok := cfg.ConnectorNeeds[entry.GraphID]; ok && len(needs) > 0 {
				metadata["connector_needs"] = needs
			}
			if cache, ok := cfg.LLMCache[entry.GraphID]; ok && cache.TTLSeconds > 0 {
				metadata["cache_ttl_seconds"] = cache.TTLSeconds
			}
			agent := &models.Agent{
				AgentID:      entry.GraphID,
				Name:         entry.GraphID,
				Description:  fmt.Sprintf("Graph loaded from %s (%s:%s)", path, entry.Path, entry.Symbol),
				Metadata:     metadata,
				Capabilities: map[string]interface{}{},
			}
			if err := store.UpsertAgent(ctx, agent); err != nil {
				slog.Error("failed to register agent", "graph_id", entry.GraphID, "error", err)
				continue
			}
			schema := &models.AgentSchema{
				AgentID:      entry.GraphID,
				InputSchema:  map[string]interface{}{"type": "object"},
				OutputSchema: map[string]interface{}{"type": "object"},
				StateSchema:  map[string]interface{}{"type": "object"},
				ConfigSchema: map[string]interface{}{"type": "object"},
			}
			_ = store.UpsertAgentSchema(ctx, schema)
			slog.Info("registered agent", "graph_id", entry.GraphID, "source", path)
		}
	}
}

func bootstrapConnectors(configPath string) *connector.Registry {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}

	for _, path := range paths {
		cfg, err := config.LoadLangGraphJSON(path)
		if err != nil {
			continue
		}
		if len(cfg.Connectors) == 0 {
			continue
		}
		configDir := filepath.Dir(path)
		connCfgs, err := config.LoadConnectorConfigs(cfg.Connectors, configDir)
		if err != nil {
			slog.Error("failed to load connector configs", "path", path, "error", err)
			continue
		}
		if len(connCfgs) > 0 {
			return connector.NewRegistry(connCfgs)
		}
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolvePort returns the flag value if set, else env var, else fallback default.
func resolvePort(flagVal, envKey, fallback string) string {
	if flagVal != "" {
		return flagVal
	}
	return envOrDefault(envKey, fallback)
}

// resolveConfig returns the flag value if set, else LANGGRAPH_CONFIG env var, else empty (auto-discover).
func resolveConfig(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return envOrDefault("LANGGRAPH_CONFIG", "")
}

// initAuthProvider reads auth config from the first langgraph.json and returns
// the appropriate auth.Provider. Returns nil (no auth) if unconfigured.
func initAuthProvider(configPath string) auth.Provider {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}

	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Auth == nil {
		return nil
	}

	switch cfg.Auth.Type {
	case "api_key":
		slog.Info("auth: api_key", "keys", len(cfg.Auth.Keys))
		return auth.NewAPIKeyProvider(cfg.Auth.Keys)

	case "jwt":
		p, err := auth.NewJWTProvider(auth.JWTConfig{
			JWKSURL:            cfg.Auth.JWKSURL,
			Issuer:             cfg.Auth.Issuer,
			Audience:           cfg.Auth.Audience,
			TenantClaim:        cfg.Auth.TenantClaim,
			ExtraClaims:        cfg.Auth.ExtraClaims,
			ForwardToken:       cfg.Auth.ForwardToken,
			RawTokenField:      cfg.Auth.RawTokenField,
			ClaimAliases:       cfg.Auth.ClaimAliases,
			ForwardHeaders:     cfg.Auth.ForwardHeaders,
			ScopeAsPermissions: cfg.Auth.ScopeAsPermissions,
		})
		if err != nil {
			slog.Error("failed to initialize JWT auth provider", "error", err)
			os.Exit(1)
		}
		slog.Info("auth: jwt", "jwks_url", cfg.Auth.JWKSURL)
		return p

	case "webhook":
		slog.Info("auth: webhook", "url", cfg.Auth.WebhookURL)
		return auth.NewWebhookProvider(auth.WebhookConfig{
			URL:             cfg.Auth.WebhookURL,
			TimeoutMs:       cfg.Auth.WebhookTimeout,
			CacheTTLSeconds: cfg.Auth.WebhookCacheTTL,
		})

	case "none", "":
		return nil

	default:
		slog.Error("unknown auth type", "type", cfg.Auth.Type)
		os.Exit(1)
		return nil
	}
}

// initAdminAuthProvider builds the independent /admin-api/* credential set
// mountPprof registers the standard net/http/pprof handlers on a custom
// mux. pprof's package normally self-registers onto http.DefaultServeMux
// as an import side effect, which doesn't help here since the control
// plane never uses that mux -- these five handlers are the same ones it
// would have registered, just attached explicitly.
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}

func initAdminAuthProvider(configPath string) auth.Provider {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Auth == nil || len(cfg.Auth.AdminKeys) == 0 {
		return nil
	}
	keys := make(map[string]auth.APIKeyEntry, len(cfg.Auth.AdminKeys))
	for key, name := range cfg.Auth.AdminKeys {
		keys[key] = auth.APIKeyEntry{Name: name, Permissions: []string{"admin"}}
	}
	slog.Info("admin auth: static keys", "count", len(keys))
	return auth.NewAPIKeyProvider(keys)
}

// initAgentAliases reads the "agent_aliases" section from the first
// discovered langgraph.json, same control-plane-wide/first-file
// convention as initA2AMaxDepth. Returns nil (meaning "no aliasing at
// all") if unconfigured.
func initAgentAliases(configPath string) map[string]config.AgentAliasEntry {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil {
		return nil
	}
	return cfg.AgentAliases
}

// initA2AMaxDepth reads the "a2a.max_depth" section from the first
// discovered langgraph.json, same control-plane-wide/first-file
// convention as initAuthProvider/initRateLimiter. Returns 0 (meaning
// "use api.Server's own default") if unconfigured or invalid.
func initA2AMaxDepth(configPath string) int {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return 0
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.A2A == nil {
		return 0
	}
	return cfg.A2A.MaxDepth
}

// initRateLimiter reads the "rate_limit" section from the first discovered
// langgraph.json, same control-plane-wide/first-file convention as
// initAuthProvider. Always returns a non-nil Limiter -- ratelimit.Limiter's
// methods are nil-safe and a Config-less Limiter is a pure pass-through, so
// callers never need to nil-check.
func initRateLimiter(configPath string) *ratelimit.Limiter {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return ratelimit.New(nil)
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.RateLimit == nil {
		return ratelimit.New(nil)
	}
	toRule := func(r *config.RateLimitRule) *ratelimit.Rule {
		if r == nil {
			return nil
		}
		return &ratelimit.Rule{RPS: r.RPS, Burst: r.Burst}
	}
	return ratelimit.New(&ratelimit.Config{
		Global:    toRule(cfg.RateLimit.Global),
		PerUser:   toRule(cfg.RateLimit.PerUser),
		PerAgent:  toRule(cfg.RateLimit.PerAgent),
		PerTenant: toRule(cfg.RateLimit.PerTenant),
	})
}

// defaultVectorDimensions is OpenAI's text-embedding-3-small/ada-002
// output size -- the most common embedding size in the wild, used when
// vector_store.dimensions is omitted.
const defaultVectorDimensions = 1536

// initVectorStore reads the "vector_store" section from the first
// discovered langgraph.json (same control-plane-wide/first-file
// convention as initRateLimiter/initHooks) and connects a pgvector-backed
// store. Returns nil if unconfigured, or if pgvector requires
// POSTGRES_DSN and it isn't set -- errors are logged, not fatal, since a
// misconfigured vector store shouldn't take down the whole control plane
// (the rest of the Agent Protocol surface has nothing to do with it).
func initVectorStore(ctx context.Context, configPath string) vectorstore.VectorStore {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.VectorStore == nil {
		return nil
	}
	dims := cfg.VectorStore.Dimensions
	if dims <= 0 {
		dims = defaultVectorDimensions
	}

	switch cfg.VectorStore.Type {
	case "pgvector":
		dsn := os.Getenv("POSTGRES_DSN")
		if dsn == "" {
			slog.Error("vector_store: type=pgvector requires POSTGRES_DSN to be set")
			return nil
		}
		vs, err := pgvector.New(ctx, dsn, dims)
		if err != nil {
			slog.Error("vector store: failed to connect", "error", err)
			return nil
		}
		if err := vs.Init(ctx); err != nil {
			slog.Error("vector store: failed to initialize schema", "error", err)
			vs.Close()
			return nil
		}
		slog.Info("vector store: pgvector", "dimensions", dims)
		return vs

	case "qdrant":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("QDRANT_URL")
		}
		if url == "" {
			slog.Error("vector_store: type=qdrant requires vector_store.url or QDRANT_URL to be set")
			return nil
		}
		vs, err := qdrant.New(url, cfg.VectorStore.Collection, dims)
		if err != nil {
			slog.Error("vector store: failed to configure qdrant", "error", err)
			return nil
		}
		if err := vs.Init(ctx); err != nil {
			slog.Error("vector store: failed to initialize qdrant collection", "error", err)
			vs.Close()
			return nil
		}
		slog.Info("vector store: qdrant", "url", url, "collection", cfg.VectorStore.Collection, "dimensions", dims)
		return vs

	default:
		slog.Error("vector_store.type not supported (only \"pgvector\" and \"qdrant\" are implemented)", "type", cfg.VectorStore.Type)
		return nil
	}
}

// initCorsConfig reads the "cors" section from the first discovered
// langgraph.json (same control-plane-wide/first-file convention as
// initRateLimiter/initHooks). Returns a disabled Config (nil-safe, adds
// no headers) if unconfigured -- correct default for server-to-server or
// same-origin deployments.
func initCorsConfig(configPath string) *cors.Config {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return &cors.Config{}
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Cors == nil {
		return &cors.Config{}
	}
	return &cors.Config{AllowOrigins: cors.ParseAllowOrigins(cfg.Cors.AllowOrigins)}
}

// initCustomRoutesProxy reads the "custom_routes" section from the first
// discovered langgraph.json (same control-plane-wide/first-file convention
// as initAuthProvider/initRateLimiter/initHooks) and builds a reverse proxy
// to it. Returns (nil, "") if unconfigured -- /custom/* then 404s, no
// background connections attempted, matching this project's
// disabled-by-default-until-configured convention for every platform
// extension.
func initCustomRoutesProxy(configPath string) (http.Handler, string) {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil, ""
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.CustomRoutes == nil || cfg.CustomRoutes.URL == "" {
		return nil, ""
	}
	target, err := url.Parse(cfg.CustomRoutes.URL)
	if err != nil {
		slog.Error("custom_routes.url is not a valid URL", "url", cfg.CustomRoutes.URL, "error", err)
		return nil, ""
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("custom route proxy error", "path", r.URL.Path, "target", target.String(), "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}
	// Strip the /custom prefix -- the user's app/sidecar sees paths as if
	// it were mounted at the root (e.g. /custom/webhook -> {url}/webhook),
	// not as if it needed to know about runkite's own mount point.
	return http.StripPrefix("/custom", proxy), target.String()
}

// initHooks reads the "webhooks" section from the first discovered
// langgraph.json (same control-plane-wide/first-file convention as
// initAuthProvider/initRateLimiter) and registers a WebhookSink per entry.
// Always returns a non-nil Dispatcher -- Dispatch/HasSinks are nil-safe and
// an empty Dispatcher is a pure no-op, so callers never need to nil-check.
func initHooks(configPath string, store state.Store) *hooks.Dispatcher {
	d := hooks.NewDispatcher()
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return d
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil {
		return d
	}
	for _, wh := range cfg.Webhooks {
		events := make([]hooks.EventType, 0, len(wh.Events))
		for _, e := range wh.Events {
			events = append(events, hooks.EventType(e))
		}
		sink := hooks.NewWebhookSink(hooks.WebhookConfig{URL: wh.URL, Secret: wh.Secret}, store)
		d.Register(sink, events...)
		slog.Info("webhook registered", "url", wh.URL, "events", wh.Events)
	}
	return d
}
