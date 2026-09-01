// Package matrix runs the real runkite binary against every meaningful
// (state backend x transport x framework runner) combination, each
// exercising the scenarios that framework's example agents actually
// support, and diffs the resulting event sequence against a recorded
// golden fixture (see golden.go).
//
// Deliberately NOT a full cross-product: backend correctness (SQLite/
// Postgres/MySQL/Mongo state; in-process/Redis transport) is already
// proven independently and exhaustively by internal/state/conformance
// and internal/transport/conformance. What THIS package proves is the
// integration seam those suites can't see -- that a real framework
// adapter's runner, talking to a real control plane over real gRPC,
// behaves identically regardless of which backend is plugged in behind
// it. Four backend combinations are enough to prove that: one
// zero-infra path (SQLite+in-process, the `runkite dev` experience most
// new users hit first) and three infra-backed combinations spanning
// every state store this project ships.
package matrix

import "time"

// BackendSpec describes one state+transport combination the control
// plane can be started with. Skip is populated lazily by requiredEnv()
// once the corresponding env var is confirmed unset, so a developer
// without the full docker-compose.test.yml stack running still gets a
// clear skip reason instead of a hang or a cryptic connection error.
type BackendSpec struct {
	Name string
	// Env returns the extra environment variables to append to the
	// control plane's process environment, or (nil, reason) if a
	// required env var isn't set (e.g. POSTGRES_DSN for postgres_redis).
	Env func() (env []string, skipReason string)
}

// allBackendSelectionVars is every env var cmd/serve.go's initStore/
// initTransport consult to choose a backend, in their exact priority
// order (POSTGRES_DSN > MYSQL_DSN > MONGO_URI > SQLite; KAFKA_URL >
// REDIS_URL > NATS_URL > in-process). A BackendSpec.Env must blank out
// every one of these it doesn't explicitly want -- appending only the
// vars a backend *wants* is not enough, because exec.Cmd.Env still
// inherits the parent test process's os.Environ() underneath, and a
// higher-priority var left over from the shell that invoked `go test`
// (e.g. `make test-matrix` exporting POSTGRES_DSN for the postgres_redis
// cell) silently wins over a lower-priority backend a DIFFERENT cell
// asked for. Caught live: without this, "sqlite_inprocess" and
// "mysql_inprocess" cells silently ran against Postgres instead
// whenever POSTGRES_DSN was set in the ambient environment (i.e.
// whenever any infra-backed backend was also being tested in the same
// run) -- same class of bug the Makefile's own `POSTGRES_DSN=
// REDIS_URL= go test ...` convention in the `test` target exists to
// prevent, just missed here on first pass since exec.Cmd's env isn't
// the same surface as a Makefile recipe's.
var allBackendSelectionVars = []string{"POSTGRES_DSN", "MYSQL_DSN", "MONGO_URI", "KAFKA_URL", "REDIS_URL", "NATS_URL"}

var backends = []BackendSpec{
	{
		Name: "sqlite_inprocess",
		Env: func() ([]string, string) {
			// ":memory:" maps to sqlitestore's empty-path convention
			// (see cmd/serve.go's initStore) -- a fresh, isolated
			// in-memory database per cell, no file cleanup needed and
			// no risk of colliding with a developer's own ./runkite.db.
			return exactEnv(map[string]string{"DATABASE_PATH": ":memory:"}), ""
		},
	},
	{
		Name: "postgres_redis",
		Env: func() ([]string, string) {
			required, skip := requiredEnv("POSTGRES_DSN", "REDIS_URL")
			if skip != "" {
				return nil, skip
			}
			return exactEnv(toMap(required)), ""
		},
	},
	{
		Name: "mysql_inprocess",
		Env: func() ([]string, string) {
			required, skip := requiredEnv("MYSQL_DSN")
			if skip != "" {
				return nil, skip
			}
			return exactEnv(toMap(required)), ""
		},
	},
	{
		Name: "mongo_redis",
		Env: func() ([]string, string) {
			required, skip := requiredEnv("MONGO_URI", "REDIS_URL")
			if skip != "" {
				return nil, skip
			}
			return exactEnv(toMap(required)), ""
		},
	},
}

// exactEnv returns a full override for every backend-selection var:
// blank for anything not in want, want's own value otherwise -- so the
// resulting env slice, appended after os.Environ(), always deterministically
// picks exactly the intended backend regardless of what the parent
// process's own environment happens to contain.
func exactEnv(want map[string]string) []string {
	env := make([]string, 0, len(allBackendSelectionVars)+len(want))
	for _, name := range allBackendSelectionVars {
		if v, ok := want[name]; ok {
			env = append(env, name+"="+v)
		} else {
			env = append(env, name+"=")
		}
	}
	for name, v := range want {
		if !containsStr(allBackendSelectionVars, name) {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func toMap(kvPairs []string) map[string]string {
	m := make(map[string]string, len(kvPairs))
	for _, kv := range kvPairs {
		for i := range kv {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return m
}

func containsStr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// requiredEnv builds a KEY=VALUE env slice from the current process
// environment for each named variable, or returns a skip reason if any
// is unset -- the same "skip entirely, don't fail" convention every
// other conformance suite in this repo uses (see e.g.
// internal/state/postgres_test.go) so this matrix behaves the same way
// locally without infra as it does in CI/nightly with it.
func requiredEnv(names ...string) (env []string, skipReason string) {
	for _, name := range names {
		v := lookupEnv(name)
		if v == "" {
			return nil, name + " not set (see `make infra-up`)"
		}
		env = append(env, name+"="+v)
	}
	return env, ""
}

// RunnerSpec describes one framework runner: how to launch its
// subprocess, which example agent(s) it registers, and which scenarios
// (from scenarios.go) it supports. Not every framework supports every
// scenario -- cancel and human-in-the-loop interrupt are LangGraph-
// specific example agents (slow_agent/approval_agent); the other four
// frameworks' adapters only have a single-shot "happy path" example
// agent today (see python/adapters/*/test_adapter.py and their generic
// event sequence of lifecycle -> values -> end), so asking for more
// than HappyPath from them would need new example agents, not new
// matrix plumbing.
type RunnerSpec struct {
	Name           string
	ConfigRelPath  string // relative to repo root
	AgentID        string // for HappyPath
	CancelAgentID  string // for Cancel, "" if unsupported
	ApprovalAgent  string // for HITL, "" if unsupported
	Scenarios      []ScenarioKind
	Launch         func(env launchEnv) (LaunchedProcess, error)
	StartupTimeout time.Duration
	// RequiredVenvRelPath, if set, is checked for existence (as
	// <path>/bin/python) before the control plane is even started --
	// CrewAI/LlamaIndex/AutoGen each need their own isolated venv (see
	// launchPythonModule's doc comment), and a developer without one set
	// up should see a clean skip with a pointer to the README, not a
	// wasted control-plane startup followed by a runner crash.
	RequiredVenvRelPath string
}

// launchEnv is everything a RunnerSpec.Launch func needs to build its
// exec.Cmd -- kept as a struct rather than positional args since the
// list has already grown once (repoRoot, configPath, grpcAddr) and will
// likely grow again.
type launchEnv struct {
	RepoRoot   string
	ConfigPath string
	GRPCAddr   string
	// HTTPAddr is the control plane's HTTP base URL for this cell
	// (e.g. http://localhost:2102). Runners need it for pre-exec status
	// checks and store proxy mode; without it they default to
	// localhost:2026, which is never where this matrix's dynamic ports
	// land.
	HTTPAddr string
}

var runners = []RunnerSpec{
	{
		Name:           "python-langgraph",
		ConfigRelPath:  "examples/all_agents/langgraph.json",
		AgentID:        "echo_agent",
		CancelAgentID:  "slow_agent",
		ApprovalAgent:  "approval_agent",
		Scenarios:      []ScenarioKind{ScenarioHappyPath, ScenarioCancel, ScenarioHITL, ScenarioHITLRestart},
		Launch:         launchPythonModule("runkite_runner", "", "python/.venv"),
		StartupTimeout: 20 * time.Second,
	},
	{
		Name:           "typescript-langgraphjs",
		ConfigRelPath:  "examples/echo_agent_ts/langgraph.json",
		AgentID:        "echo_agent_ts",
		CancelAgentID:  "slow_agent_ts",
		ApprovalAgent:  "approval_agent_ts",
		// Same LangGraph HITL surface as python-langgraph: proxy opaque
		// blobs (no runner POSTGRES_DSN) must survive interrupt → resume
		// and interrupt → kill runner → resume (P3 parity with P2a-4/5).
		Scenarios:      []ScenarioKind{ScenarioHappyPath, ScenarioCancel, ScenarioHITL, ScenarioHITLRestart},
		Launch:         launchTypeScriptRunner,
		StartupTimeout: 25 * time.Second, // tsx's first-run TS transform is slower than a warm Python import
	},
	{
		Name:           "python-langchain",
		ConfigRelPath:  "examples/langchain_agent/langgraph.json",
		AgentID:        "langchain_agent",
		Scenarios:      []ScenarioKind{ScenarioHappyPath},
		Launch:         launchPythonModule("langchain_adapter", "python/adapters", "python/.venv"),
		StartupTimeout: 20 * time.Second,
	},
	{
		Name:                "python-crewai",
		ConfigRelPath:       "examples/crewai_agent/langgraph.json",
		AgentID:             "crewai_agent",
		Scenarios:           []ScenarioKind{ScenarioHappyPath},
		Launch:              launchPythonModule("crewai_adapter", "python/adapters", "python/adapters/crewai_adapter/.venv"),
		StartupTimeout:      30 * time.Second, // real crewai package import is noticeably heavier than a fake-LLM chain
		RequiredVenvRelPath: "python/adapters/crewai_adapter/.venv",
	},
	{
		Name:                "python-llamaindex",
		ConfigRelPath:       "examples/llamaindex_agent/langgraph.json",
		AgentID:             "llamaindex_agent",
		Scenarios:           []ScenarioKind{ScenarioHappyPath},
		Launch:              launchPythonModule("llamaindex_adapter", "python/adapters", "python/adapters/llamaindex_adapter/.venv"),
		StartupTimeout:      30 * time.Second,
		RequiredVenvRelPath: "python/adapters/llamaindex_adapter/.venv",
	},
	{
		Name:                "python-autogen",
		ConfigRelPath:       "examples/autogen_agent/langgraph.json",
		AgentID:             "autogen_greeter",
		Scenarios:           []ScenarioKind{ScenarioHappyPath},
		Launch:              launchPythonModule("autogen_adapter", "python/adapters", "python/adapters/autogen_adapter/.venv"),
		StartupTimeout:      30 * time.Second,
		RequiredVenvRelPath: "python/adapters/autogen_adapter/.venv",
	},
}
