package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/nats-io/nats.go"
	goredis "github.com/redis/go-redis/v9"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/cors"
	"github.com/getrunkite/runkite/internal/customroutes"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/secureheaders"
	"github.com/getrunkite/runkite/internal/state"
	mongostore "github.com/getrunkite/runkite/internal/state/mongo"
	mysqlstore "github.com/getrunkite/runkite/internal/state/mysql"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/tracing"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	kafkatransport "github.com/getrunkite/runkite/internal/transport/kafka"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	"github.com/getrunkite/runkite/internal/vectorstore"
	pgvector "github.com/getrunkite/runkite/internal/vectorstore/pgvector"
	pineconestore "github.com/getrunkite/runkite/internal/vectorstore/pinecone"
	qdrant "github.com/getrunkite/runkite/internal/vectorstore/qdrant"
	weaviatestore "github.com/getrunkite/runkite/internal/vectorstore/weaviate"
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

// checkProductionAdmission fails closed (loud ERROR + os.Exit(1)) when
// `serve` -- never `dev`, which explicitly opts into the zero-dependency
// local experience -- would otherwise start with a posture that looks
// healthy but silently isn't production-safe: a non-durable single-node
// store, no shared transport, or no way to tell a stranger's request
// apart from an operator's own.
//
// Found in an external audit and confirmed live: `runkite serve` with
// no env vars at all falls all the way through to SQLite + in-process
// transport + "runner auth: disabled (local mode)" + a fully open
// /admin-api/*, logs everything at INFO, and never refuses to start.
// That's the right default for `dev`; for the command literally named
// `serve`, a missing env var in a deploy manifest deserves a hard stop,
// not a quietly-degraded production instance passing its own /readyz.
//
// RUNKITE_ALLOW_INSECURE_SERVE=1 is the escape hatch -- a private
// network, CI, or a deliberate quick demo are all legitimate reasons to
// run `serve` without every box checked; this only removes the
// "nobody meant to do this" failure mode, not the ability to do it on
// purpose. Every e2e/matrix test harness in test/e2e/ sets this env var
// for exactly that reason -- they intentionally run minimal/local
// backends and no auth to isolate what they're actually testing.
func checkProductionAdmission(opts serverOpts) {
	problems := admissionProblems(opts)
	if len(problems) == 0 {
		return
	}

	slog.Error("refusing to start `serve` with an insecure default posture -- set RUNKITE_ALLOW_INSECURE_SERVE=1 to start anyway (e.g. a private network, CI, or a deliberate quick demo), or use `runkite dev` for the zero-dependency local experience")
	for _, p := range problems {
		slog.Error("  - " + p)
	}
	os.Exit(1)
}

// admissionProblems is checkProductionAdmission's pure decision logic,
// split out so it's unit-testable without exiting the test binary
// itself (checkProductionAdmission's os.Exit(1) can't be exercised
// in-process). Returns nil (no problems) for opts.devMode, the
// RUNKITE_MODE=test / RUNKITE_ALLOW_INSECURE_SERVE=1 bypasses, or a
// fully-configured serve; otherwise one string per unmet requirement.
func admissionProblems(opts serverOpts) []string {
	if opts.devMode {
		return nil
	}
	if envOrDefault("RUNKITE_MODE", "") == "test" || os.Getenv("RUNKITE_ALLOW_INSECURE_SERVE") != "" {
		return nil
	}

	var problems []string
	if os.Getenv("POSTGRES_DSN") == "" && os.Getenv("MYSQL_DSN") == "" && os.Getenv("MONGO_URI") == "" {
		problems = append(problems, "no durable state backend configured (POSTGRES_DSN, MYSQL_DSN, or MONGO_URI) -- serve would default to a single-node SQLite file with no HA and no multi-replica safety")
	}
	if os.Getenv("REDIS_URL") == "" && os.Getenv("NATS_URL") == "" && os.Getenv("KAFKA_URL") == "" {
		problems = append(problems, "no shared transport configured (REDIS_URL, NATS_URL, or KAFKA_URL) -- serve would default to in-process transport, which cannot coordinate multiple replicas at all")
	}
	if !auth.LoadRunnerTokensFromEnv().Enabled() {
		problems = append(problems, "no RUNNER_TOKEN_* configured -- any process that can reach the gRPC bridge would be trusted as a runner")
	}
	if !hasClientFacingAuthConfigured(opts.configPath) {
		problems = append(problems, `no client-facing auth configured ("auth" in langgraph.json) -- every REST/WebSocket/admin-api endpoint would be open with no credentials required`)
	}
	return problems
}

// hasClientFacingAuthConfigured reports whether either a primary auth
// provider or admin_keys is configured -- mirrors initAuthProvider's/
// initAdminAuthProvider's own config lookup rather than calling them
// directly, since constructing a real auth.Provider here (which
// checkProductionAdmission runs before any other initialization) would
// duplicate work startServer already does moments later.
func hasClientFacingAuthConfigured(configPath string) bool {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return false
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Auth == nil {
		return false
	}
	return cfg.Auth.Type != "" || len(cfg.Auth.AdminKeys) > 0
}

// authStrictPermissions reads auth.strict_permissions from the first
// discovered langgraph.json (same first-file convention as
// initAuthProvider). Unset + api_key/jwt/webhook defaults to true --
// see config.AuthEntry.EffectiveStrictPermissions.
func authStrictPermissions(configPath string) bool {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return false
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Auth == nil {
		return false
	}
	return cfg.Auth.EffectiveStrictPermissions()
}

func startServer(opts serverOpts) {
	setupLogging()
	checkProductionAdmission(opts)

	// Cancelable, not context.Background(): every background loop below
	// (queue-depth poller, stale-job reclaimer, cron scheduler, retention,
	// run timeout, store TTL sweep) already selects on ctx.Done() to know when to stop
	// -- confirmed live before this fix that none of them ever actually
	// saw that signal, because nothing ever cancelled this context; they
	// only ever stopped via the process dying under them at os.Exit.
	// cancel() is called explicitly in the shutdown sequence at the
	// bottom of this function, not deferred -- this function used to
	// never return normally at all (a bare blocking ListenAndServe call,
	// or os.Exit from the old signal handler), so a defer here would
	// never have run either.
	ctx, cancel := context.WithCancel(context.Background())

	// OTel observability fan-out. No-op, zero overhead, and no
	// background connection attempts unless
	// OTEL_EXPORTER_OTLP_ENDPOINT is set -- see internal/tracing.
	shutdownTracing, err := tracing.Init(ctx)
	if err != nil {
		slog.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	// --- State store ---
	store := initStore(ctx)
	warnCheckpointDualMode()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Transport layer ---
	// One shared Redis client when REDIS_URL is set: transport (queue/
	// broker/cancelbus) and the optional redis rate-limit backend both
	// reuse it rather than opening a second connection pool.
	rdb := initRedis(ctx)
	queue, broker, cancelBus := initTransport(ctx, rdb)

	// runkite_queue_depth is a gauge, not something incremented at call
	// sites -- it must be polled periodically to reflect current state.
	go pollQueueDepth(ctx, queue)

	// Reclaim jobs dequeued by a runner that then died before Ack
	// (zombie GetJob long-poll / crash between Dequeue and first event).
	// Passes rdb so Kafka+Redis HA can elect a single reclaim leader
	// (see tryAcquireReclaimLeader) -- closes Kafka's dual-reaper
	// double-dispatch window when Redis is present.
	go reclaimStaleJobs(ctx, queue, rdb)

	// Bootstrap agents and connectors from langgraph.json files
	bootstrapAgents(store, opts.configPath)

	// HTTP API server
	apiServer := api.NewServer(store, queue, broker, cancelBus)

	// Wire connector registry if configured
	if reg := bootstrapConnectors(opts.configPath); reg != nil {
		apiServer.SetConnectorRegistry(reg)
		slog.Info("connector registry loaded", "connectors", reg.List())
	}

	// Vector/semantic store. Disabled entirely unless a vector_store
	// section is present -- never
	// implicitly enabled just because POSTGRES_DSN is set, since an
	// existing Postgres deployment may not have the pgvector extension
	// installed or permitted.
	if vs := initVectorStore(ctx, opts.configPath); vs != nil {
		apiServer.SetVectorStore(vs)
		// initVectorStore already logs the specific backend/type/config
		// it selected -- no separate log line needed here.
	}

	// Rate limiting: per-user, per-agent, per-tenant, configurable via
	// config. Disabled (zero overhead) unless a rate_limit section is
	// present. Uses rdb when backend is redis / auto-selected.
	rateLimiter := initRateLimiter(opts.configPath, rdb)
	apiServer.SetRateLimiter(rateLimiter)
	if rateLimiter.Enabled() {
		slog.Info("rate limiting: enabled", "backend", rateLimiter.BackendName())
	}

	// Agent-to-Agent (A2A) delegation recursion depth limit. Always
	// available at POST /internal/a2a/runs; this only tunes how deep a
	// chain may go.
	if maxDepth := initA2AMaxDepth(opts.configPath); maxDepth > 0 {
		apiServer.SetA2AMaxDepth(maxDepth)
		slog.Info("a2a: max_depth configured", "max_depth", maxDepth)
	}

	// A/B deployment routing, built on top of full agent versioning.
	// Disabled (pure pass-through) unless "agent_aliases" is configured.
	if aliasCfg := initAgentAliases(opts.configPath); len(aliasCfg) > 0 {
		apiServer.SetAliasResolver(api.NewAliasResolver(aliasCfg))
		slog.Info("agent aliases: configured", "count", len(aliasCfg))
	}

	// Event hooks + webhook delivery: on_run_start, on_run_complete,
	// on_tool_call, on_error, on_interrupt.
	hookDispatcher := initHooks(opts.configPath, store)
	apiServer.SetHookDispatcher(hookDispatcher)
	if hookDispatcher.HasSinks() {
		slog.Info("event hooks: enabled")
	}

	// Custom routes: in-runner + sidecar modes -- both are a reverse
	// proxy from here, see internal/customroutes.
	if proxy, url, mount := initCustomRoutesProxy(opts.configPath); proxy != nil {
		apiServer.SetCustomRoutesProxy(proxy, mount)
		slog.Info("custom routes: enabled", "proxy_url", url, "mount", mount+"/*")
	}

	// Cron scheduler: cron-expression scheduling with multi-instance-safe
	// claiming (Postgres claim window) and timezone support. Schedules
	// are bootstrapped the same way agents are (every
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
		slog.Info("retention: enabled", "runs_max_age", retentionCfg.runsMaxAge, "checkpoints_keep_last", retentionCfg.checkpointsKeepLast, "cron_claims_max_age", retentionCfg.cronClaimsMaxAge, "terminal_hook_claims_max_age", retentionCfg.terminalHookClaimsMaxAge, "interval", retentionCfg.interval)
		if retentionCfg.checkpointsKeepLast > 0 {
			// LangGraph's AsyncPostgresSaver / PostgresSaver tables are a
			// separate schema from thread_checkpoints -- pruning the
			// latter never touches the former (see README Checkpoint
			// dual mode / Retention).
			slog.Warn("retention: checkpoints_keep_last only prunes runkite thread_checkpoints (proxy-mode history); LangGraph direct-mode tables (checkpoints/checkpoint_blobs/checkpoint_writes) are not touched")
		}
		go runRetentionLoop(ctx, store, retentionCfg)
	}

	// Run timeout sweep (opt-in): forces pending/running runs older than
	// max_duration to status "timeout". Distinct from crash reclaim --
	// reclaim covers a dead runner; this covers a live hung one. Absent
	// "run_timeout" config starts no background loop at all.
	if timeoutCfg := initRunTimeoutConfig(opts.configPath); timeoutCfg != nil {
		slog.Info("run_timeout: enabled", "max_duration", timeoutCfg.maxDuration, "interval", timeoutCfg.interval)
		go runTimeoutLoop(ctx, apiServer, timeoutCfg)
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

	// Runner auth uses a two-tier model: local mode (no RUNNER_TOKEN_*
	// env vars) trusts runners implicitly, zero setup.
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
	// TLS/mTLS for the gRPC bridge: off by default (plaintext, same as
	// always) unless GRPC_TLS_CERT_FILE/GRPC_TLS_KEY_FILE are set.
	// GRPC_TLS_CLIENT_CA_FILE additionally requires and verifies a
	// runner's own client certificate (mTLS) -- a second, independent
	// trust boundary from RUNNER_TOKEN_* above: a stolen/guessed bearer
	// token no longer suffices on its own to open a gRPC connection at
	// all when this is set, since the TLS handshake itself now rejects
	// any client that can't present a certificate signed by this CA,
	// before the token interceptor ever runs.
	grpcTLSConfig, err := serverTLSConfig(
		os.Getenv("GRPC_TLS_CERT_FILE"), os.Getenv("GRPC_TLS_KEY_FILE"), os.Getenv("GRPC_TLS_CLIENT_CA_FILE"),
	)
	if err != nil {
		slog.Error("invalid gRPC TLS configuration", "error", err)
		os.Exit(1)
	}
	grpcOpts := []grpc.ServerOption{
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
	}
	if grpcTLSConfig != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(grpcTLSConfig)))
		slog.Info("gRPC bridge: TLS enabled", "mtls", grpcTLSConfig.ClientCAs != nil)
	} else {
		slog.Info("gRPC bridge: TLS disabled (plaintext)")
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	pb.RegisterRunnerServiceServer(grpcServer, bridgeServer)

	grpcErrCh := make(chan error, 1)
	go func() {
		slog.Info("gRPC bridge listening", "port", opts.grpcPort)
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			grpcErrCh <- err
		}
	}()

	// Auth middleware wraps the API handler; /metrics is outside auth.
	// ratelimit.Middleware sits inside auth (not outside) so it can read
	// the identity auth.Middleware just set in the request context for
	// per-user limiting -- global/per-user only; per-agent limiting happens
	// separately inside createRun, see internal/ratelimit's doc comment.
	authProvider := initAuthProvider(opts.configPath)
	adminAuthProvider := initAdminAuthProvider(opts.configPath)
	authOpts := auth.MiddlewareOpts{StrictPermissions: authStrictPermissions(opts.configPath)}
	if authOpts.StrictPermissions {
		slog.Info("auth: strict_permissions enabled (empty permissions deny)")
	}
	rateLimited := ratelimit.Middleware(rateLimiter, apiServer.Handler())
	authedAPI := auth.MiddlewareWithOpts(authProvider, adminAuthProvider, runnerTokens, authOpts, rateLimited)

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
	// force real CPU load for 30s. Requires RUNKITE_PPROF_TOKEN so the
	// endpoints are not anonymously reachable when enabled (Bearer or
	// X-Runkite-Pprof-Token). Registered outside client auth so a
	// profiling run isn't gated by the Agent Protocol credential set.
	if os.Getenv("RUNKITE_PPROF") == "1" {
		if token := os.Getenv("RUNKITE_PPROF_TOKEN"); token != "" {
			mountPprof(root, token)
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
			slog.Info("pprof: enabled at /debug/pprof/ (RUNKITE_PPROF=1, token required)", "mutex_block_profiling", true)
		} else {
			slog.Warn("pprof: RUNKITE_PPROF=1 set but RUNKITE_PPROF_TOKEN is empty; not mounting /debug/pprof/")
		}
	}
	root.Handle("/", authedAPI)

	// CORS: without this, a browser-based frontend on a different
	// origin can't reach the control plane at all -- the
	// browser blocks it client-side before auth or any handler ever
	// runs). Wraps the whole root, outside auth, so an OPTIONS preflight
	// (which carries no Authorization header by design) is answered
	// directly instead of being rejected by auth.Middleware.
	corsCfg := initCorsConfig(opts.configPath)
	corsed := cors.Middleware(corsCfg, root)
	if corsCfg.Enabled() {
		slog.Info("cors: enabled", "allow_origins", corsCfg.AllowOrigins)
	}

	// Security headers (CSP, nosniff, frame deny, …) -- always on, outside
	// auth, so public paths like /admin/ and /health get them too. See
	// internal/secureheaders. HSTS deliberately omitted (TLS often
	// terminates at a reverse proxy).
	secured := secureheaders.Middleware(corsed)

	// Metrics middleware wraps the entire root (including /metrics routing).
	// otelhttp is outermost so every request gets a trace context (and
	// otelhttp uses httpsnoop internally, which dynamically preserves
	// whatever optional interfaces -- Flusher for SSE, Hijacker for
	// WebSocket -- the writer beneath it implements, so it can't reintroduce
	// the SSE-breaking bug metrics.responseWriter had before its own
	// Flush()/Unwrap() fix).
	handler := metrics.HTTPMiddleware(secured)
	handler = otelhttp.NewHandler(handler, "runkite-http")

	// Start HTTP server
	// hostname is logged (not a new config knob) specifically so multiple
	// replicas behind a load balancer are distinguishable in shared logs --
	// Docker/K8s already give each container/pod a unique hostname, so this
	// is free instance identification with zero new configuration surface.
	hostname, _ := os.Hostname()
	slog.Info("control plane starting", "http_port", opts.httpPort, "grpc_port", opts.grpcPort, "hostname", hostname)

	// TLS/mTLS for the client-facing HTTP API: off by default (plain
	// HTTP, same as always) unless TLS_CERT_FILE/TLS_KEY_FILE are set.
	// TLS_CLIENT_CA_FILE additionally requires and verifies a client
	// certificate (mTLS) -- independent of whatever client-facing auth
	// provider is configured above; the two compose (a request still
	// needs both a valid client cert AND a valid credential the auth
	// provider accepts, if both are configured). Loaded here, before
	// the startup banner below, purely so the banner can print the
	// right scheme (https vs http) -- ListenAndServeTLS itself isn't
	// called until the goroutine further down.
	httpTLSConfig, err := serverTLSConfig(
		os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE"), os.Getenv("TLS_CLIENT_CA_FILE"),
	)
	if err != nil {
		slog.Error("invalid HTTP TLS configuration", "error", err)
		os.Exit(1)
	}
	scheme := "http"
	if httpTLSConfig != nil {
		scheme = "https"
	}

	if opts.devMode {
		fmt.Printf("\n  Runkite Control Plane (dev)\n")
	} else {
		fmt.Printf("\n  Runkite Control Plane\n")
	}
	fmt.Printf("  HTTP API:    %s://localhost:%s\n", scheme, opts.httpPort)
	fmt.Printf("  gRPC bridge: localhost:%s\n", opts.grpcPort)
	fmt.Printf("  Admin UI:    %s://localhost:%s/admin/\n", scheme, opts.httpPort)
	fmt.Printf("  Health:      %s://localhost:%s/health\n", scheme, opts.httpPort)
	fmt.Printf("  Metrics:     %s://localhost:%s/metrics\n\n", scheme, opts.httpPort)

	// ReadHeaderTimeout (not the full ReadTimeout/WriteTimeout) is the
	// one server-level timeout safe to set unconditionally here:
	// ReadTimeout bounds an entire request's read (including the body),
	// and WriteTimeout bounds the entire response write -- both would
	// silently kill this project's own legitimately long-lived
	// connections (SSE run/thread streams that stay open for a whole
	// run's duration, WebSocket, GetJob-style long-polls proxied through
	// custom routes) if set to anything short enough to matter as a
	// hardening measure. ReadHeaderTimeout only bounds the time to read
	// the request line + headers, before any of that starts -- it closes
	// the classic Slowloris-style slow-header attack without touching
	// any of those long-lived cases at all. IdleTimeout bounds how long
	// a kept-alive connection can sit between requests, which similarly
	// never applies to an actively-streaming one.
	srv := &http.Server{
		Addr:              ":" + opts.httpPort,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         httpTLSConfig,
	}

	httpErrCh := make(chan error, 1)
	go func() {
		// Empty cert/key filenames here are correct, not a bug: when
		// httpTLSConfig is non-nil, its own Certificates field (loaded
		// by serverTLSConfig) is already populated, and
		// ListenAndServeTLS's own doc comment confirms filenames are
		// only required when TLSConfig.Certificates/GetCertificate
		// are NOT already set.
		var serveErr error
		if httpTLSConfig != nil {
			slog.Info("HTTP API: TLS enabled", "mtls", httpTLSConfig.ClientCAs != nil)
			serveErr = srv.ListenAndServeTLS("", "")
		} else {
			slog.Info("HTTP API: TLS disabled (plaintext)")
			serveErr = srv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			httpErrCh <- serveErr
		}
	}()

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received, draining", "signal", sig.String())
	case err := <-httpErrCh:
		slog.Error("HTTP server failed", "error", err)
	case err := <-grpcErrCh:
		slog.Error("gRPC server failed", "error", err)
	}

	// Stop background loops (queue-depth poller, stale-job reclaimer,
	// cron scheduler, retention, run timeout, store TTL sweep) immediately -- none of
	// them serve a live client request, so there's nothing to drain by
	// letting them keep ticking during shutdown.
	cancel()

	// 15s total, shared across the HTTP drain below AND the gRPC drain
	// after it -- deliberately under Kubernetes's 30s default
	// terminationGracePeriodSeconds (SIGKILL follows after that), but
	// still longer than Docker Compose's own 10s default
	// stop_grace_period, which is why docker-compose.yml and
	// docker-compose.multi.yml both set stop_grace_period explicitly on
	// this service -- without that override Compose would SIGKILL
	// mid-drain before this budget is ever used.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()

	// HTTP first: stops accepting new connections and waits for
	// in-flight ones (including long-lived SSE/WebSocket) to finish on
	// their own, up to shutdownCtx's own deadline -- a real, deliberate
	// trade-off for a client mid-stream when that deadline is hit: the
	// connection is forcibly closed rather than the process hanging
	// indefinitely waiting for it (e.g. a truly stuck client, or a run
	// that legitimately takes longer than any reasonable shutdown grace
	// period). Before gRPC: a runner's in-flight GetJob long-poll or
	// StreamEvents call doesn't depend on the HTTP server at all, so
	// ordering between the two here doesn't matter for correctness, only
	// for which one's own drain gets more of the shared deadline first.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown did not complete cleanly", "error", err)
	}

	// GracefulStop blocks until every in-flight RPC finishes, with NO
	// timeout of its own -- a runner's own GetJob long-poll (up to ~30s
	// by design, see the keepalive comment above) or a StreamEvents call
	// for a long-running agent could otherwise hold this open far longer
	// than shutdownCtx's own budget. Racing it against that same
	// deadline and falling back to Stop() (forceful: aborts every
	// in-flight RPC immediately) is the standard grpc-go pattern for a
	// bounded graceful shutdown.
	grpcStopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(grpcStopped)
	}()
	select {
	case <-grpcStopped:
	case <-shutdownCtx.Done():
		slog.Warn("gRPC graceful stop did not complete in time, forcing")
		grpcServer.Stop()
	}

	// A fresh, separate 5s budget -- same reasoning as tracing's own
	// fresh context just below: shutdownCtx is already spent by the
	// HTTP+gRPC drain above in the common case, and a webhook delivery
	// already queued or in flight when the signal arrived (e.g. from a
	// run that completed during the drain window) deserves the same
	// "let it finish or run out of budget" treatment the rest of this
	// sequence gives every other kind of in-flight work -- see
	// hooks.Dispatcher.Close's own doc comment for what happened before
	// this existed (queued/in-flight deliveries simply died with the
	// process, no different from a hard kill).
	hookCloseCtx, cancelHookClose := context.WithTimeout(context.Background(), 5*time.Second)
	if err := hookDispatcher.Close(hookCloseCtx); err != nil {
		slog.Warn("webhook dispatcher did not drain in time", "error", err)
	}
	cancelHookClose()

	// Deliberately a FRESH context here, not shutdownCtx -- by this
	// point shutdownCtx has already been spent (often almost entirely)
	// by the HTTP+gRPC drain above, and the connected-runner case is
	// the COMMON one, not an edge case (see the gRPC comment above:
	// GracefulStop reliably runs close to the full budget whenever a
	// runner's long-lived WatchCancels stream is open). Reusing
	// shutdownCtx here would mean the tracing flush -- the entire
	// reason a signal handler exists in the first place, see
	// tracing.Init's own doc comment on why OTel's buffered spans are
	// silently dropped without an explicit flush -- runs against an
	// already-expired deadline in the typical case, silently skipping
	// exactly the export it was added to guarantee.
	tracingCtx, cancelTracing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTracing()
	if err := shutdownTracing(tracingCtx); err != nil {
		slog.Error("tracing shutdown error", "error", err)
	}
	_ = store.Close()
	slog.Info("shutdown complete")
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

// --- Shared helpers ---

// warnCheckpointDualMode logs when the control-plane state backend is
// not Postgres. Runner POSTGRES_DSN enables LangGraph direct-mode
// checkpoints (and direct store_items access) against Postgres -- a
// separate database from MySQL/Mongo/SQLite control planes. Operators
// who leave POSTGRES_DSN set on runners in that topology get a silent
// split: agent state in one DB, control-plane metadata in another.
func warnCheckpointDualMode() {
	if msg := checkpointDualModeWarning(); msg != "" {
		slog.Warn(msg)
	}
}

// checkpointDualModeWarning returns the warn text for the active state
// backend selection, or "" when Postgres is in use (Supported direct-
// mode pairing). Extracted for tests without slog capture.
func checkpointDualModeWarning() string {
	if os.Getenv("POSTGRES_DSN") != "" {
		return ""
	}
	backend := "sqlite"
	switch {
	case os.Getenv("MYSQL_DSN") != "":
		backend = "mysql"
	case os.Getenv("MONGO_URI") != "":
		backend = "mongodb"
	}
	return "state store is " + backend + ": runner POSTGRES_DSN enables LangGraph direct-mode checkpoints/store against Postgres, which is a different database from this control plane. For " + backend + " control planes, unset POSTGRES_DSN on runners and set RUNKITE_HTTP_URL for store proxy mode; durable LangGraph checkpoints require a Postgres control plane (Supported profile). See README Checkpoint dual mode."
}

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

	// MySQL: the second SQL exemplar alongside Postgres/SQLite -- a
	// drop-in SQL twin when a deployment needs MySQL rather than
	// Postgres (see internal/state/mysql's package doc). Checked after
	// POSTGRES_DSN, same precedence convention as MONGO_URI below, so
	// setting multiple backend env vars at once is deterministic, not a
	// race.
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

	// MongoDB: the project's non-SQL exemplar backend, proving that
	// state.Store is genuinely implementable against a document store,
	// not just SQL -- see internal/state/mongo's package doc.
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

// initRedis returns a shared go-redis client when REDIS_URL is set, or
// nil otherwise. Pinged at construction so a bad URL fails loud at
// startup rather than on the first Allow/Enqueue. Callers (transport,
// rate limiter) must not Close it for the process lifetime.
func initRedis(ctx context.Context) *goredis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return nil
	}
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
	return rdb
}

func initTransport(ctx context.Context, rdb *goredis.Client) (transport.JobQueue, transport.EventBroker, transport.CancelBroker) {
	// Kafka is a JobQueue-only backend (see internal/transport/kafka's
	// own package doc for why) -- KAFKA_URL only ever picks the queue,
	// paired with Redis's own EventBroker/CancelBroker when REDIS_URL
	// is also set, or the in-process ones otherwise. Checked before the
	// Redis-only branch below so "both set" means "Kafka queue, Redis
	// broker/cancelbus," not "Redis for everything."
	if kafkaURL := os.Getenv("KAFKA_URL"); kafkaURL != "" {
		var kafkaOpts []kafkatransport.Option
		// KAFKA_JOB_PARTITIONS: see kafkatransport.WithJobPartitions's
		// own doc comment for why this matters -- the default (1) means
		// only one control-plane replica ever actively dequeues a given
		// runner_kind at a time. Set consistently across every replica
		// sharing this Kafka cluster; only affects a job topic's
		// partition count the first time it's created.
		if partitionsStr := os.Getenv("KAFKA_JOB_PARTITIONS"); partitionsStr != "" {
			n, err := strconv.Atoi(partitionsStr)
			if err != nil || n <= 0 {
				slog.Error("invalid KAFKA_JOB_PARTITIONS, must be a positive integer", "value", partitionsStr)
				os.Exit(1)
			}
			kafkaOpts = append(kafkaOpts, kafkatransport.WithJobPartitions(n))
		}
		queue, err := kafkatransport.NewQueue(ctx, strings.Split(kafkaURL, ","), kafkaOpts...)
		if err != nil {
			slog.Error("failed to initialize kafka job queue", "error", err)
			os.Exit(1)
		}
		if rdb != nil {
			broker := redistransport.NewBroker(rdb)
			cancelBus := redistransport.NewCancelBus(rdb)
			slog.Info("transport: kafka queue + redis broker/cancelbus", "kafka_url", kafkaURL, "redis_url", os.Getenv("REDIS_URL"))
			return queue, broker, cancelBus
		}
		// Kafka without Redis: events/cancel are process-local, and
		// reclaim has no cluster-wide leader lock -- fine for a single
		// control-plane process, not a supported multi-replica HA story.
		slog.Warn("transport: kafka queue + in-process broker/cancelbus (REDIS_URL unset -- single-instance only; multi-replica reclaim can double-dispatch)")
		slog.Info("transport: kafka queue + in-process broker/cancelbus", "kafka_url", kafkaURL)
		return queue, inprocess.NewBroker(), inprocess.NewCancelBus()
	}

	if rdb != nil {
		queue := redistransport.NewQueue(rdb)
		broker := redistransport.NewBroker(rdb)
		cancelBus := redistransport.NewCancelBus(rdb)
		slog.Info("transport: redis", "url", os.Getenv("REDIS_URL"))
		return queue, broker, cancelBus
	}

	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		nc, err := nats.Connect(natsURL)
		if err != nil {
			slog.Error("failed to connect to nats", "error", err)
			os.Exit(1)
		}
		queue, err := natstransport.NewQueue(ctx, nc)
		if err != nil {
			slog.Error("failed to initialize nats job queue", "error", err)
			os.Exit(1)
		}
		broker, err := natstransport.NewBroker(ctx, nc)
		if err != nil {
			slog.Error("failed to initialize nats event broker", "error", err)
			os.Exit(1)
		}
		cancelBus := natstransport.NewCancelBus(nc)
		slog.Info("transport: nats", "url", natsURL)
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

// mountPprof registers the standard net/http/pprof handlers on a custom
// mux, each gated by token (Bearer or X-Runkite-Pprof-Token). pprof's
// package normally self-registers onto http.DefaultServeMux as an import
// side effect, which doesn't help here since the control plane never
// uses that mux.
func mountPprof(mux *http.ServeMux, token string) {
	gate := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Runkite-Pprof-Token")
			if got == "" {
				if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
					got = strings.TrimPrefix(authz, "Bearer ")
				}
			}
			// Constant-time compare: opt-in ops endpoint, but still a
			// shared secret -- avoid short-circuit string equality.
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /debug/pprof/", gate(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", gate(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", gate(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", gate(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", gate(pprof.Trace))
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
//
// Backend selection (cfg.RateLimit.Backend):
//   - "redis" -- shared Lua token buckets on rdb; exits if rdb is nil
//   - "memory" -- process-local buckets (explicit opt-out of sharing)
//   - "" (omit) -- auto: redis when rdb != nil, otherwise memory
//
// Auto exists so a multi-replica compose with REDIS_URL + rate_limit gets
// a shared ceiling without an extra config knob; set "backend":"memory"
// to keep the old per-process behavior even when Redis is present.
func initRateLimiter(configPath string, rdb *goredis.Client) *ratelimit.Limiter {
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
	rlCfg := &ratelimit.Config{
		Backend:   strings.ToLower(strings.TrimSpace(cfg.RateLimit.Backend)),
		Global:    toRule(cfg.RateLimit.Global),
		PerUser:   toRule(cfg.RateLimit.PerUser),
		PerAgent:  toRule(cfg.RateLimit.PerAgent),
		PerTenant: toRule(cfg.RateLimit.PerTenant),
	}

	useRedis, missingRedis, unknown := rateLimitBackendChoice(rlCfg.Backend, rdb != nil)
	if missingRedis {
		slog.Error("rate_limit.backend=redis requires REDIS_URL")
		os.Exit(1)
	}
	if unknown != "" {
		slog.Error("rate_limit: unknown backend, falling back to memory", "backend", unknown)
	}

	if useRedis {
		return ratelimit.NewRedis(rlCfg, rdb)
	}
	return ratelimit.New(rlCfg)
}

// rateLimitBackendChoice is initRateLimiter's pure decision logic for
// which store to use -- split out so the "backend=redis without REDIS_URL
// must fail" path is unit-testable without os.Exit (same pattern as
// admissionProblems). Returns useRedis, missingRedis (caller should exit),
// and unknown (non-empty backend name when unrecognized).
func rateLimitBackendChoice(backend string, hasRedis bool) (useRedis bool, missingRedis bool, unknown string) {
	b := strings.ToLower(strings.TrimSpace(backend))
	switch b {
	case "redis":
		if !hasRedis {
			return false, true, ""
		}
		return true, false, ""
	case "memory":
		return false, false, ""
	case "":
		return hasRedis, false, ""
	default:
		return false, false, b
	}
}

// defaultVectorDimensions is a common text-embedding width (1536), used
// when vector_store.dimensions is omitted.
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

	case "weaviate":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("WEAVIATE_URL")
		}
		if url == "" {
			slog.Error("vector_store: type=weaviate requires vector_store.url or WEAVIATE_URL to be set")
			return nil
		}
		vs, err := weaviatestore.New(url, cfg.VectorStore.Class, dims)
		if err != nil {
			slog.Error("vector store: failed to configure weaviate", "error", err)
			return nil
		}
		if err := vs.Init(ctx); err != nil {
			slog.Error("vector store: failed to initialize weaviate class", "error", err)
			vs.Close()
			return nil
		}
		slog.Info("vector store: weaviate", "url", url, "class", cfg.VectorStore.Class, "dimensions", dims)
		return vs

	case "pinecone":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("PINECONE_URL")
		}
		if url == "" {
			// Pinecone's real control plane has one fixed, well-known
			// host -- unlike Qdrant/Weaviate, there's no "unconfigured"
			// error case here, just a default a real account needs
			// (Pinecone Local users set vector_store.url/PINECONE_URL
			// explicitly instead).
			url = "https://api.pinecone.io"
		}
		apiKey := cfg.VectorStore.APIKey
		if apiKey == "" {
			apiKey = os.Getenv("PINECONE_API_KEY")
		}
		if apiKey == "" && url == "https://api.pinecone.io" {
			slog.Error("vector_store: type=pinecone requires vector_store.api_key or PINECONE_API_KEY when using the real Pinecone service (set vector_store.url for Pinecone Local instead)")
			return nil
		}
		vs, err := pineconestore.New(url, apiKey, cfg.VectorStore.Index, dims, cfg.VectorStore.Cloud, cfg.VectorStore.Region)
		if err != nil {
			slog.Error("vector store: failed to configure pinecone", "error", err)
			return nil
		}
		if err := vs.Init(ctx); err != nil {
			slog.Error("vector store: failed to initialize pinecone index", "error", err)
			vs.Close()
			return nil
		}
		slog.Info("vector store: pinecone", "url", url, "index", cfg.VectorStore.Index, "dimensions", dims)
		return vs

	default:
		slog.Error("vector_store.type not supported (only \"pgvector\", \"qdrant\", \"weaviate\", and \"pinecone\" are implemented)", "type", cfg.VectorStore.Type)
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
// to it. Returns (nil, "", "") if unconfigured -- the mount then 404s, no
// background connections attempted, matching this project's
// disabled-by-default-until-configured convention for every platform
// extension.
func initCustomRoutesProxy(configPath string) (http.Handler, string, string) {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil, "", ""
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.CustomRoutes == nil || cfg.CustomRoutes.URL == "" {
		return nil, "", ""
	}
	target, err := url.Parse(cfg.CustomRoutes.URL)
	if err != nil {
		slog.Error("custom_routes.url is not a valid URL", "url", cfg.CustomRoutes.URL, "error", err)
		return nil, "", ""
	}
	mount, err := customroutes.NormalizeMount(cfg.CustomRoutes.Mount)
	if err != nil {
		slog.Error("custom_routes.mount is invalid", "mount", cfg.CustomRoutes.Mount, "error", err)
		return nil, "", ""
	}
	proxy, err := customroutes.NewProxy(target, mount)
	if err != nil {
		slog.Error("custom_routes proxy setup failed", "error", err)
		return nil, "", ""
	}
	return proxy, target.String(), mount
}

// initHooks reads the "webhooks" and "preflight_hooks" sections from the
// first discovered langgraph.json (same control-plane-wide/first-file
// convention as initAuthProvider/initRateLimiter). Webhooks are async
// observational sinks; preflight_hooks are sync gates that can deny run
// creation. Always returns a non-nil Dispatcher -- Dispatch/HasSinks/
// CheckBeforeRun are nil-safe and an empty Dispatcher is a pure no-op.
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
	for _, pf := range cfg.PreflightHooks {
		timeout := hooks.DefaultPreflightTimeout
		if pf.TimeoutMS > 0 {
			timeout = time.Duration(pf.TimeoutMS) * time.Millisecond
		}
		gate := hooks.NewWebhookGate(hooks.WebhookGateConfig{
			URL: pf.URL, Secret: pf.Secret, Timeout: timeout,
		})
		d.RegisterGate(gate)
		slog.Info("preflight hook registered", "url", pf.URL, "timeout", timeout)
	}
	return d
}
