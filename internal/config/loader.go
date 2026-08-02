// Package config handles loading and parsing langgraph.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
)

// LangGraphConfig represents a parsed langgraph.json file.
type LangGraphConfig struct {
	Graphs       map[string]string `json:"graphs"`       // graph_id -> "path:symbol"
	Dependencies []string          `json:"dependencies"` // relative paths
	// RunnerKind identifies which runner implementation should execute
	// every graph declared in THIS file -- the Runner Protocol is
	// language-agnostic, and a real deployment can mix runners, some
	// agents Python, some TypeScript, routed by this field. One config
	// file maps to one runner process reading it, so this is file-level,
	// not per-graph like ConnectorNeeds/LLMCache below. Defaults to
	// "python-langgraph" when omitted -- every langgraph.json written
	// before this field existed keeps working unchanged.
	RunnerKind string                    `json:"runner_kind,omitempty"`
	Auth       *AuthEntry                `json:"auth,omitempty"`
	Connectors map[string]ConnectorEntry `json:"connectors,omitempty"`
	// ConnectorNeeds declares, per graph_id, which registered connectors that
	// agent needs pre-warmed sessions for (RunAssignment.connector_needs).
	// Additive/optional -- absent means no pre-warm hints for that agent.
	ConnectorNeeds map[string][]string `json:"connector_needs,omitempty"`
	// RateLimit is control-plane-wide, like Auth -- read from the first
	// discovered langgraph.json only (see initRateLimiter in cmd/serve.go),
	// same convention as Auth.
	RateLimit *RateLimitEntry `json:"rate_limit,omitempty"`
	// Webhooks is control-plane-wide, same first-file convention as Auth
	// and RateLimit (see initHooks in cmd/serve.go). Observational only
	// (async); cannot reject runs. For sync deny-before-create guardrails,
	// use PreflightHooks.
	Webhooks []WebhookEntry `json:"webhooks,omitempty"`
	// PreflightHooks is control-plane-wide, same first-file convention as
	// Webhooks. Each entry is a synchronous HTTP gate consulted before a
	// run is claimed/created; deny or timeout fails closed (403).
	PreflightHooks []PreflightHookEntry `json:"preflight_hooks,omitempty"`
	// LLMCache declares, per graph_id, that agent's whole-run result cache
	// TTL, for LLM response caching. Same per-agent,
	// metadata-embedded-at-bootstrap convention as ConnectorNeeds --
	// absent means caching is off for that agent (never on by default:
	// an agent with side effects, e.g. "send_email", must never have its
	// result cached and replayed for a repeated input).
	LLMCache map[string]CacheEntry `json:"llm_cache,omitempty"`
	// CustomRoutes is control-plane-wide, same first-file convention as
	// Auth/RateLimit/Webhooks (see initCustomRoutesProxy in cmd/serve.go).
	CustomRoutes *CustomRoutesEntry `json:"custom_routes,omitempty"`
	// Cron declares scheduled runs. Keyed by schedule name (must be
	// unique across every discovered langgraph.json, unlike per-agent
	// maps -- schedules from every config file are bootstrapped
	// together, not just the first one, same as Graphs).
	Cron map[string]CronEntry `json:"cron,omitempty"`
	// VectorStore is control-plane-wide, same first-file convention as
	// Auth/RateLimit/Webhooks/CustomRoutes (see initVectorStore in
	// cmd/serve.go). Disabled entirely when absent -- never implicitly
	// enabled just because POSTGRES_DSN is set; the vector/semantic
	// store is None (disabled) by default.
	VectorStore *VectorStoreEntry `json:"vector_store,omitempty"`
	// Cors is control-plane-wide, same first-file convention as
	// Auth/RateLimit/Webhooks (see initCorsMiddleware in cmd/serve.go).
	// Absent means no CORS headers are added at all -- correct for
	// server-to-server or same-origin deployments, but a browser-based
	// frontend on a different origin (the common case: a Vite/React dev
	// server on its own port) needs this configured, or every request
	// silently fails the browser's CORS preflight before it ever reaches
	// application logic (this looks nothing like an auth or network
	// error -- the browser blocks it client-side).
	Cors *CorsEntry `json:"cors,omitempty"`
	// Retention is control-plane-wide, same first-file convention as
	// Auth/RateLimit/Webhooks (see initRetention in cmd/retention.go).
	// Absent means no automatic cleanup at all -- runs/checkpoints
	// persist forever until an explicit DELETE /threads/{id}, the
	// pre-existing behavior (real gap found reviewing the Admin UI:
	// unbounded storage growth with no way to bound it short of manual
	// deletion). Opt-in, same convention as everything else here.
	Retention *RetentionEntry `json:"retention,omitempty"`
	// RunTimeout is control-plane-wide, same first-file convention as
	// Retention (see initRunTimeoutConfig in cmd/run_timeout.go).
	// Absent means no automatic run deadline -- a hung agent (alive but
	// stuck in an infinite tool loop) stays pending/running forever
	// unless a client cancels it. Distinct from crash reclaim (heartbeat
	// lease): reclaim covers a dead runner; this covers a live one that
	// never finishes.
	RunTimeout *RunTimeoutEntry `json:"run_timeout,omitempty"`
	// A2A is control-plane-wide, same first-file convention as
	// Auth/RateLimit/Webhooks (see initA2AConfig in cmd/serve.go).
	// Absent means the default max depth (see A2AEntry.MaxDepth)
	// applies -- A2A delegation itself (POST /internal/a2a/runs) is
	// always available, this section only tunes the depth limit.
	A2A *A2AEntry `json:"a2a,omitempty"`
	// AgentAliases is control-plane-wide, same first-file convention as
	// Auth/RateLimit/A2A (see initAgentAliases in cmd/serve.go). Absent
	// means no alias exists at all -- an agent_id that happens to match
	// a real registered agent runs it directly, same as always; a
	// client-facing name that ISN'T a real registered agent and also
	// isn't an alias 404s, same as always.
	AgentAliases map[string]AgentAliasEntry `json:"agent_aliases,omitempty"`
}

// AgentAliasEntry is one entry in langgraph.json's "agent_aliases"
// section, part of full agent versioning's A/B deployment routing.
// A client-facing name (the map key in AgentAliases) that
// probabilistically resolves to one of several REAL registered
// agent_ids at run-creation time -- e.g. rolling out a new graph
// version to 10% of traffic while 90% still gets the stable one, both
// deployed as ordinary, independently-registered agents.
type AgentAliasEntry struct {
	// Targets maps a real agent_id to its relative weight. Weights
	// don't need to sum to 100 -- they're normalized against their own
	// sum (so {"a": 1, "b": 1} and {"a": 50, "b": 50} both mean a 50/50
	// split), matching how most weighted-routing configs work rather
	// than requiring the operator to do the arithmetic themselves.
	Targets map[string]int `json:"targets"`
}

// A2AEntry is the "a2a" section of langgraph.json, for Agent-to-Agent
// (A2A) delegation where an agent calls another agent via the same Agent
// Protocol API. Currently just the one knob every deployment needs to
// consider: how deep a delegation chain is allowed to go before the
// control plane refuses to create another sub-run, the guard against a
// runaway or cyclic A->B->A delegation loop.
type A2AEntry struct {
	// MaxDepth caps how many delegation hops a chain may have (a
	// top-level run is depth 0; each POST /internal/a2a/runs call
	// increments it by one from its parent). Defaults to 10 when unset
	// or <= 0 -- generous enough for real multi-agent workflows,
	// small enough that a cyclic delegation bug fails fast instead of
	// silently consuming resources.
	MaxDepth int `json:"max_depth,omitempty"`
}

// RunTimeoutEntry is the "run_timeout" section of langgraph.json.
type RunTimeoutEntry struct {
	// MaxDuration is how long a run may stay pending or running before
	// the control plane forces it to status "timeout". Go duration
	// string (e.g. "30m", "2h"). Required for the section to take
	// effect -- absent/empty/invalid disables the sweep entirely.
	MaxDuration string `json:"max_duration,omitempty"`
	// IntervalSeconds controls how often the background sweep looks
	// for overdue runs. Defaults to 15 if omitted or <= 0.
	IntervalSeconds int `json:"interval_seconds,omitempty"`
}

// RetentionEntry is the "retention" section of langgraph.json.
type RetentionEntry struct {
	// RunsMaxAge prunes TERMINAL runs (success/error/interrupted/timeout
	// -- never pending/running) whose updated_at is older than this. Go
	// duration string (e.g. "720h" for 30 days). Absent/empty disables
	// run pruning.
	RunsMaxAge string `json:"runs_max_age,omitempty"`
	// CheckpointsKeepLast keeps only this many most recent checkpoints
	// per thread (never touches a thread's own current-state snapshot,
	// only its history). Absent/zero/negative disables checkpoint
	// pruning -- there is deliberately no way to write a config that
	// means "prune everything."
	CheckpointsKeepLast int `json:"checkpoints_keep_last,omitempty"`
	// CronClaimsMaxAge prunes cron_claims rows (README's "Cron's claim
	// table has no retention sweep" gap) with fire_time older than this.
	// Same Go duration string format as RunsMaxAge; independently
	// optional -- a deployment with no cron schedules configured never
	// needs this.
	CronClaimsMaxAge string `json:"cron_claims_max_age,omitempty"`
	// TerminalHookClaimsMaxAge prunes terminal_hook_claims rows (the
	// multi-replica terminal-webhook exactly-once table) with claimed_at
	// older than this. Same Go duration string format as RunsMaxAge;
	// independently optional -- a deployment with no webhooks configured
	// never writes this table (finishRun gates on HasSinks).
	TerminalHookClaimsMaxAge string `json:"terminal_hook_claims_max_age,omitempty"`
	// WebhookDeadLettersMaxAge prunes webhook_dead_letters rows whose
	// failed_at is older than this. Same Go duration string format as
	// RunsMaxAge; independently optional.
	WebhookDeadLettersMaxAge string `json:"webhook_dead_letters_max_age,omitempty"`
	// IntervalMinutes controls how often the background prune loop
	// runs. Defaults to 60 (once per hour) if any field above is set
	// but this is omitted.
	IntervalMinutes int `json:"interval_minutes,omitempty"`
}

// CorsEntry is the "cors" section of langgraph.json ({"allow_origins":
// [...]}) -- a plain browser/HTTP standard concern, unrelated to any
// specific server implementation. Kept as a simple, familiar shape so
// migrating a browser-based frontend's CORS config from another
// LangGraph SDK-compatible server costs nothing.
type CorsEntry struct {
	AllowOrigins []string `json:"allow_origins"`
}

// VectorStoreEntry is the "vector_store" section of langgraph.json, the
// vector/semantic store config. "pgvector" (Tier 1, SQL-based),
// "qdrant", "weaviate", and "pinecone" (the non-SQL exemplars, same
// role Mongo plays for state.Store) are all implemented now -- every
// backend named in the original plan is built.
type VectorStoreEntry struct {
	Type string `json:"type"` // "pgvector" | "qdrant" | "weaviate" | "pinecone"
	// Dimensions fixes the embedding vector's width (pgvector's
	// vector(N) column / Qdrant's collection vector size / Pinecone's
	// index dimension are all fixed-dimension; Weaviate has no such
	// native constraint, so that package enforces it itself instead --
	// see internal/vectorstore/weaviate's own doc comment). Defaults to
	// 1536 (a common text-embedding width) when omitted
	// -- every item upserted must supply exactly this many floats.
	Dimensions int `json:"dimensions,omitempty"`
	// URL is Qdrant's, Weaviate's, or Pinecone's REST base URL (e.g.
	// "http://localhost:6333" / "http://localhost:8080" /
	// "https://api.pinecone.io"), read from QDRANT_URL / WEAVIATE_URL /
	// PINECONE_URL if unset here -- same env-var-first, config-second
	// convention as POSTGRES_DSN for pgvector. Ignored for type=pgvector.
	// For type=pinecone specifically, defaults to Pinecone's own fixed
	// control-plane host (https://api.pinecone.io) if left entirely
	// unset -- only override this to point at a self-hosted Pinecone
	// Local instance for development/testing.
	URL string `json:"url,omitempty"`
	// Collection names the Qdrant collection every tenant/namespace
	// shares (see internal/vectorstore/qdrant's package doc for why one
	// shared collection, not one per tenant/namespace). Defaults to
	// "vector_items". Ignored for type=pgvector/weaviate/pinecone.
	Collection string `json:"collection,omitempty"`
	// Class names the Weaviate class every tenant/namespace shares --
	// Weaviate's own naming rule (must start with an uppercase letter)
	// applies. Defaults to "VectorItems". Ignored for
	// type=pgvector/qdrant/pinecone.
	Class string `json:"class,omitempty"`
	// Index names the Pinecone index every tenant/namespace shares --
	// Pinecone's own naming rule (lowercase alphanumeric/hyphens only)
	// applies. Defaults to "vector-items". Ignored for
	// type=pgvector/qdrant/weaviate.
	Index string `json:"index,omitempty"`
	// APIKey authenticates against a real Pinecone account, read from
	// PINECONE_API_KEY if unset here -- never write a real key directly
	// into langgraph.json, same secrets-via-env convention as
	// RUNNER_TOKEN/POSTGRES_DSN elsewhere. Ignored (and unnecessary) for
	// Pinecone Local, which doesn't authenticate requests at all.
	// Ignored for every other type.
	APIKey string `json:"api_key,omitempty"`
	// Cloud/Region place a real Pinecone serverless index at creation
	// time (e.g. "aws"/"us-east-1", Pinecone's own documented example
	// values, which is also this package's default when both are left
	// empty). Meaningless for Pinecone Local, which accepts and ignores
	// any value here. Ignored for every other type.
	Cloud  string `json:"cloud,omitempty"`
	Region string `json:"region,omitempty"`
}

// CronEntry is one entry in langgraph.json's "cron" section.
type CronEntry struct {
	AgentID    string                 `json:"agent_id"`
	Expression string                 `json:"expression"`         // standard 5-field cron expression
	Timezone   string                 `json:"timezone,omitempty"` // IANA name; default UTC
	Input      map[string]interface{} `json:"input,omitempty"`
	Config     map[string]interface{} `json:"config,omitempty"`
	Enabled    *bool                  `json:"enabled,omitempty"` // nil defaults to true
}

// CustomRoutesEntry configures the "Custom routes" platform
// extension: user-defined HTTP endpoints mounted at /custom/*. From the
// control plane's side, in-runner mode (the Python runner SDK hosts the
// user's ASGI app itself, see python/runkite_runner/custom_app.py) and
// sidecar mode (a user-run, language-agnostic process) are the exact same
// mechanism -- a reverse proxy to a URL. URL is where either kind of
// process is expected to be listening.
// CustomRoutesEntry mounts a product-owned HTTP app beside the Agent
// Protocol API. The control plane reverse-proxies Mount/* to URL and
// strips Mount before forwarding (see internal/customroutes).
type CustomRoutesEntry struct {
	URL string `json:"url"`
	// Mount is the external URL prefix (default "/custom"). Use a
	// product-specific prefix (e.g. "/sales-assistant") when the
	// frontend already calls bare product paths and you don't want a
	// /custom shim. Must not collide with Agent Protocol reserved paths.
	Mount string `json:"mount,omitempty"`
}

// CacheEntry configures whole-run result caching for one agent.
type CacheEntry struct {
	TTLSeconds int `json:"ttl_seconds"`
}

// WebhookEntry is one entry in langgraph.json's "webhooks" array —
// async observational delivery on lifecycle events, with retry and
// dead-letter (see internal/hooks.WebhookSink).
type WebhookEntry struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"` // HMAC-SHA256 signing secret, sent as X-Runkite-Signature
	Events []string `json:"events,omitempty"` // event type names (run_start, run_complete, tool_call, error, interrupt); empty means all
}

// PreflightHookEntry is one entry in langgraph.json's "preflight_hooks"
// array — a sync HTTP gate that can allow or deny run creation.
type PreflightHookEntry struct {
	URL       string `json:"url"`
	Secret    string `json:"secret,omitempty"`     // HMAC-SHA256; same X-Runkite-Signature as webhooks
	TimeoutMS int    `json:"timeout_ms,omitempty"` // per-request timeout; default 2000
}

// RateLimitEntry is the "rate_limit" section of langgraph.json. Any
// subset of dimensions may be set; the rest are unlimited.
type RateLimitEntry struct {
	// Backend selects the store: "memory" (process-local token buckets),
	// "redis" (shared via REDIS_URL across replicas), or omit for auto
	// (redis when REDIS_URL is set, otherwise memory). See
	// internal/ratelimit and initRateLimiter in cmd/serve.go.
	Backend   string         `json:"backend,omitempty"`
	Global    *RateLimitRule `json:"global,omitempty"`
	PerUser   *RateLimitRule `json:"per_user,omitempty"`
	PerAgent  *RateLimitRule `json:"per_agent,omitempty"`
	PerTenant *RateLimitRule `json:"per_tenant,omitempty"` // wired in internal/ratelimit.AllowTenant
}

// RateLimitRule configures one token bucket.
type RateLimitRule struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

// AuthEntry is the "auth" section of langgraph.json.
type AuthEntry struct {
	Type        string `json:"type"` // none, api_key, jwt, webhook
	JWKSURL     string `json:"jwks_url,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	Audience    string `json:"audience,omitempty"`
	TenantClaim string `json:"tenant_claim,omitempty"` // JWT only; default "tenant_id"
	// ExtraClaims/ForwardToken/RawTokenField are JWT-only -- see
	// auth.JWTConfig's doc comments for the full rationale (Factory
	// Graph runtime.user needing more than identity/permissions/
	// tenant_id, e.g. a raw bearer token for downstream token exchange).
	ExtraClaims        []string                    `json:"extra_claims,omitempty"`
	ForwardToken       bool                        `json:"forward_token,omitempty"`
	RawTokenField      string                      `json:"raw_token_field,omitempty"`
	ClaimAliases       map[string]string           `json:"claim_aliases,omitempty"`
	ForwardHeaders     map[string]string           `json:"forward_headers,omitempty"`
	ScopeAsPermissions bool                        `json:"scope_as_permissions,omitempty"`
	Keys               map[string]auth.APIKeyEntry `json:"keys,omitempty"`
	WebhookURL         string                      `json:"url,omitempty"`
	WebhookTimeout     int                         `json:"timeout_ms,omitempty"`
	WebhookCacheTTL    int                         `json:"cache_ttl_seconds,omitempty"`
	// StrictPermissions fail-closes authorization when a credential
	// authenticates with an empty app-level permissions list (nil, [],
	// or only foreign IdP scopes that filterAppPermissions drops).
	// Default false keeps the backward-compatible "empty = unrestricted"
	// convention; production SSO/api_key deployments that want every
	// caller to carry explicit read/write/admin should set this true.
	StrictPermissions bool `json:"strict_permissions,omitempty"`
	// AdminKeys defines an independent credential set accepted ONLY for
	// /admin-api/*, regardless of Type above (map: key string -> a
	// display name for it, shown nowhere but useful in audit logging).
	// Every key implicitly grants "admin" -- there's no reason to hold
	// one otherwise. Orthogonal to the primary provider so a deployment
	// using short-lived SSO tokens (Type: "jwt") for real end users
	// doesn't force an operator to keep re-authenticating just to view
	// the dashboard; a normal SSO token that itself carries "admin"
	// still works too (see auth.Middleware's doc comment).
	AdminKeys map[string]string `json:"admin_keys,omitempty"`
}

// ConnectorEntry is a connector definition in langgraph.json.
// Either inline config or a config_ref to an external YAML file.
type ConnectorEntry struct {
	ConfigRef      string                          `json:"config_ref,omitempty"`
	Auth           *connector.AuthConfig           `json:"auth,omitempty"`
	MCP            *connector.MCPConfig            `json:"mcp,omitempty"`
	Errors         map[string]string               `json:"errors,omitempty"`
	Tools          *connector.ToolFilter           `json:"tools,omitempty"`
	CircuitBreaker *connector.CircuitBreakerConfig `json:"circuit_breaker,omitempty"`
}

// LoadConnectorConfigs resolves ConnectorEntry items into ConnectorConfig map.
// configDir is the directory containing langgraph.json (used to resolve config_ref paths).
func LoadConnectorConfigs(entries map[string]ConnectorEntry, configDir string) (map[string]connector.ConnectorConfig, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	result := make(map[string]connector.ConnectorConfig, len(entries))
	for name, entry := range entries {
		if entry.ConfigRef != "" {
			refPath := entry.ConfigRef
			if !filepath.IsAbs(refPath) {
				refPath = filepath.Join(configDir, refPath)
			}
			cfg, err := loadConnectorYAML(refPath)
			if err != nil {
				return nil, fmt.Errorf("connector %s: %w", name, err)
			}
			result[name] = *cfg
		} else if entry.Auth != nil {
			result[name] = connector.ConnectorConfig{
				Auth:           *entry.Auth,
				MCP:            entry.MCP,
				Errors:         entry.Errors,
				Tools:          entry.Tools,
				CircuitBreaker: entry.CircuitBreaker,
			}
		} else {
			return nil, fmt.Errorf("connector %s: must have auth or config_ref", name)
		}
	}
	return result, nil
}

// loadConnectorYAML reads a connector config from a YAML or JSON file.
// gopkg.in/yaml.v3 parses both real YAML syntax and plain JSON (a valid
// subset of YAML 1.2), so a single Unmarshal call handles either format.
func loadConnectorYAML(path string) (*connector.ConnectorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read connector config: %w", err)
	}
	// Same ${VAR}/${VAR:-default} substitution as langgraph.json -- a
	// connector's own auth credentials (API keys, client secrets) are
	// exactly the kind of thing that shouldn't live in a checked-in
	// file either. expandEnvVars operates on raw bytes, so it works
	// identically for this YAML-or-JSON file.
	data, err = expandEnvVars(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cfg connector.ConnectorConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse connector config %s: %w", path, err)
	}
	return &cfg, nil
}

// GraphEntry is a parsed graph reference from the config.
type GraphEntry struct {
	GraphID string // key in the "graphs" map
	Path    string // file path portion before ":"
	Symbol  string // symbol name after ":"
}

// envVarPattern matches ${VAR_NAME} and ${VAR_NAME:-default}, the same
// syntax docker-compose/Makefiles/POSIX shell parameter expansion use --
// deliberately familiar rather than inventing new config syntax.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// expandEnvVars substitutes ${VAR} / ${VAR:-default} placeholders in raw
// langgraph.json bytes with process environment values, so secrets
// (admin_keys, api_key entries, webhook URLs, JWKS endpoints, ...) never
// have to live in the checked-in config file: deploy tooling (Helm, a
// Vault Agent sidecar, plain `envsubst`, a local .env) sets the real env
// vars, and langgraph.json itself stays a safe-to-commit template.
//
// ${VAR} with no default and an unset *or empty* env var is a hard
// error -- fail fast at config-load time on a missing secret rather
// than silently substituting an empty string into something like
// jwks_url or an admin key, where "empty" is a valid-looking but
// badly broken value. (os.LookupEnv reports ok=true for a var set to
// ""; treating that as success would create e.g. admin_keys[""].)
// Use ${VAR:-default} when empty is intentionally allowed.
func expandEnvVars(data []byte) ([]byte, error) {
	var missing []string
	expanded := envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		groups := envVarPattern.FindSubmatch(match)
		name := string(groups[1])
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return []byte(v)
		}
		if len(groups[2]) > 0 {
			return []byte(strings.TrimPrefix(string(groups[2]), ":-"))
		}
		missing = append(missing, name)
		return match
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("references undefined environment variable(s) with no default: %s (use ${%s:-default} to allow one)", strings.Join(missing, ", "), missing[0])
	}
	return expanded, nil
}

// LoadLangGraphJSON reads and parses a langgraph.json file.
func LoadLangGraphJSON(path string) (*LangGraphConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read langgraph.json: %w", err)
	}
	data, err = expandEnvVars(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cfg LangGraphConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse langgraph.json: %w", err)
	}
	if len(cfg.Graphs) == 0 {
		return nil, fmt.Errorf("langgraph.json: no graphs defined")
	}
	if cfg.RunnerKind == "" {
		cfg.RunnerKind = "python-langgraph"
	}
	return &cfg, nil
}

// ParseGraphEntries extracts graph entries from the config.
func (c *LangGraphConfig) ParseGraphEntries() ([]GraphEntry, error) {
	var entries []GraphEntry
	for id, ref := range c.Graphs {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("graph %q: invalid reference %q (expected path:symbol)", id, ref)
		}
		entries = append(entries, GraphEntry{
			GraphID: id,
			Path:    parts[0],
			Symbol:  parts[1],
		})
	}
	return entries, nil
}

// FindLangGraphJSON searches for langgraph.json in common locations.
// Priority: explicit path > CWD > examples/*/langgraph.json
func FindLangGraphJSON(explicit string) []string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return []string{explicit}
		}
		return nil
	}

	var found []string

	// Check CWD
	if _, err := os.Stat("langgraph.json"); err == nil {
		found = append(found, "langgraph.json")
	}

	// Check examples/*/langgraph.json
	matches, _ := filepath.Glob("examples/*/langgraph.json")
	found = append(found, matches...)

	return found
}
