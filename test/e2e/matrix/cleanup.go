package matrix

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// matrixAgentIDs is every graph_id any RunnerSpec's config registers at
// control-plane startup -- NOT just the ones a scenario actually
// dispatches a run against. A langgraph.json's entire "graphs" section
// gets registered regardless of which single agent a test scenario
// exercises (e.g. python-langgraph's all_agents config registers
// react_agent and store_agent too, even though only echo/slow/approval
// have scenarios here), so cleanup must match that full set exactly or
// it silently leaves rows behind.
var matrixAgentIDs = []string{
	"echo_agent", "slow_agent", "approval_agent", "react_agent", "store_agent", // python-langgraph (all_agents)
	"echo_agent_ts", "slow_agent_ts", "approval_agent_ts", "factory_agent_ts", "llm_sim_agent_ts", // typescript-langgraphjs
	"langchain_agent",  // python-langchain
	"crewai_agent",     // python-crewai
	"llamaindex_agent", // python-llamaindex
	"autogen_greeter",  // python-autogen
}

// cleanupSharedBackends deletes every agent (and its versions/schemas)
// this matrix ever registers from whichever persistent, shared backend
// databases were actually used (postgres_redis/mysql_inprocess/
// mongo_redis) -- NOT from sqlite_inprocess, which is a fresh :memory:
// instance per cell and needs no cleanup at all.
//
// Why this exists: unlike sqlite_inprocess, Postgres/MySQL/Mongo point
// at the SAME persistent test database every run (docker-compose.test.yml's
// long-lived containers), under the same "default" tenant every other
// e2e suite in this repo also uses. Caught live: leaving these agent
// rows behind broke ../e2e_test.go and ../adapters/main_test.go's own
// waitForAgents, because /agents/search defaults to a 10-result page
// (internal/api/agents.go's clampSearchLimit) and this matrix's ~15
// distinct agent_ids, once accumulated, alphabetically crowded out
// whatever agent those sibling suites were actually polling for. This
// runs once after the whole matrix (TestMain, not per-cell) since it's
// a global, not per-cell, side effect to undo.
func cleanupSharedBackends() {
	ctx := context.Background()
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		if err := cleanupSQL(ctx, "pgx", dsn, "$1", true); err != nil {
			fmt.Fprintf(os.Stderr, "test/e2e/matrix: postgres cleanup failed (non-fatal): %v\n", err)
		}
	}
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		if err := cleanupSQL(ctx, "mysql", dsn, "?", false); err != nil {
			fmt.Fprintf(os.Stderr, "test/e2e/matrix: mysql cleanup failed (non-fatal): %v\n", err)
		}
	}
	if uri := os.Getenv("MONGO_URI"); uri != "" {
		if err := cleanupMongo(ctx, uri); err != nil {
			fmt.Fprintf(os.Stderr, "test/e2e/matrix: mongo cleanup failed (non-fatal): %v\n", err)
		}
	}
	// Redis is shared across every redis-transport cell and across
	// successive `go test -count=N` / local matrix invocations. Leftover
	// queue / inflight / cancel keys from a prior cell (or a crashed
	// run) make HITL resume flake: the runner dequeues a stale job and
	// the graph re-interrupts instead of applying Command(resume=...).
	// Caught live as "hitl fails after happy_path/cancel on mongo_redis
	// but passes alone on a flushed Redis". Flush only rk:* so we do
	// not touch unrelated keys if someone pointed REDIS_URL at a shared
	// instance.
	if url := os.Getenv("REDIS_URL"); url != "" {
		if err := cleanupRedis(ctx, url); err != nil {
			fmt.Fprintf(os.Stderr, "test/e2e/matrix: redis cleanup failed (non-fatal): %v\n", err)
		}
	}
}

// cleanupRedis deletes every rk:* key (queues, inflight zset/hashes,
// cancel markers). SCAN + DEL rather than FLUSHALL so a mis-pointed
// REDIS_URL cannot wipe an entire shared Redis.
func cleanupRedis(ctx context.Context, redisURL string) error {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(cctx).Err(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	var cursor uint64
	for {
		keys, next, err := rdb.Scan(cctx, cursor, "rk:*", 200).Result()
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if len(keys) > 0 {
			if err := rdb.Del(cctx, keys...).Err(); err != nil {
				return fmt.Errorf("del: %w", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

// cleanupSQL handles both Postgres (pgx, "$1"-style placeholders, IN
// (...) works fine since pgx expands a []string arg to ANY($1) itself
// -- see the arrayPlaceholder branch) and MySQL (mysql driver,
// "?"-style, one placeholder per value since it can't expand a slice).
func cleanupSQL(ctx context.Context, driverName, dsn, placeholderStyle string, isPostgres bool) error {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	var query string
	var args []any
	if isPostgres {
		query = "agent_id = ANY(" + placeholderStyle + ")"
		args = []any{matrixAgentIDs}
	} else {
		placeholders := make([]string, len(matrixAgentIDs))
		args = make([]any, len(matrixAgentIDs))
		for i, id := range matrixAgentIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		query = "agent_id IN (" + strings.Join(placeholders, ",") + ")"
	}

	for _, table := range []string{"agent_versions", "agent_schemas", "agents"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'default' AND "+query, args...); err != nil {
			return fmt.Errorf("delete from %s: %w", table, err)
		}
	}
	return nil
}

func cleanupMongo(ctx context.Context, uri string) error {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	db := client.Database(envOrDefault("MONGO_DB", "runkite"))
	filter := bson.M{"tenant_id": "default", "agent_id": bson.M{"$in": matrixAgentIDs}}
	for _, coll := range []string{"agent_versions", "agent_schemas", "agents"} {
		if _, err := db.Collection(coll).DeleteMany(ctx, filter); err != nil {
			return fmt.Errorf("delete from %s: %w", coll, err)
		}
	}
	return nil
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
