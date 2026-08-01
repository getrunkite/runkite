package main

import (
	"context"
	"os"
	"testing"

	mysqlstore "github.com/getrunkite/runkite/internal/state/mysql"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
)

// initStore calls os.Exit(1) on a connection failure, so these tests
// only exercise the success path against real, already-running test
// infra (same convention as every other backend's own conformance
// test skipping when its env var/service isn't available) -- there's
// no way to assert the os.Exit(1) failure path in-process without a
// subprocess trick that isn't worth the complexity here.
//
// DSNs are read from the SAME env vars initStore itself reads
// (POSTGRES_DSN/MYSQL_DSN/MONGO_URI), falling back to
// docker-compose.test.yml's local dev ports (5433/3307/27018) only
// when unset -- NOT hardcoded to those local ports unconditionally.
// CI (.github/workflows/ci.yml) runs its postgres/mysql/mongo services
// on their standard ports (5432/3306/27017) instead, specifically to
// avoid colliding with a developer's own local services; hardcoding
// the local dev ports here would make every test below silently skip
// in CI forever while still passing locally -- exactly the kind of gap
// that looks green but tests nothing where it matters most.
func dsnOrDefault(envVar, localDefault string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return localDefault
}

func TestInitStore_MySQLDSNSelectsMySQLBackend(t *testing.T) {
	dsn := dsnOrDefault("MYSQL_DSN", "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true")
	if _, err := mysqlstore.New(context.Background(), dsn); err != nil {
		t.Skipf("mysql not available: %v", err)
	}

	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("MYSQL_DSN", dsn)
	t.Setenv("MONGO_URI", "")

	s := initStore(context.Background())
	defer s.Close()

	if _, ok := s.(*mysqlstore.Store); !ok {
		t.Fatalf("expected MYSQL_DSN to select the MySQL backend, got %T", s)
	}
}

// TestInitStore_PostgresTakesPrecedenceOverMySQLAndMongo proves the
// documented precedence order (POSTGRES_DSN > MYSQL_DSN > MONGO_URI >
// SQLite fallback) actually holds in code, not just in a comment --
// setting all three backend env vars at once must be deterministic.
func TestInitStore_PostgresTakesPrecedenceOverMySQLAndMongo(t *testing.T) {
	pgDSN := dsnOrDefault("POSTGRES_DSN", "postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable")
	if _, err := pgstore.New(context.Background(), pgDSN); err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	mysqlDSN := dsnOrDefault("MYSQL_DSN", "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true")
	mongoURI := dsnOrDefault("MONGO_URI", "mongodb://127.0.0.1:27018/?replicaSet=rs0&directConnection=true")

	t.Setenv("POSTGRES_DSN", pgDSN)
	t.Setenv("MYSQL_DSN", mysqlDSN)
	t.Setenv("MONGO_URI", mongoURI)

	s := initStore(context.Background())
	defer s.Close()

	if _, ok := s.(*pgstore.Store); !ok {
		t.Fatalf("expected POSTGRES_DSN to take precedence over MYSQL_DSN/MONGO_URI, got %T", s)
	}
}

// TestInitStore_MySQLTakesPrecedenceOverMongo proves the second half of
// the same precedence order: with POSTGRES_DSN unset but both MYSQL_DSN
// and MONGO_URI set, MySQL wins (checked first in initStore).
func TestInitStore_MySQLTakesPrecedenceOverMongo(t *testing.T) {
	mysqlDSN := dsnOrDefault("MYSQL_DSN", "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true")
	if _, err := mysqlstore.New(context.Background(), mysqlDSN); err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	mongoURI := dsnOrDefault("MONGO_URI", "mongodb://127.0.0.1:27018/?replicaSet=rs0&directConnection=true")

	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("MYSQL_DSN", mysqlDSN)
	t.Setenv("MONGO_URI", mongoURI)

	s := initStore(context.Background())
	defer s.Close()

	if _, ok := s.(*mysqlstore.Store); !ok {
		t.Fatalf("expected MYSQL_DSN to take precedence over MONGO_URI, got %T", s)
	}
}

// TestInitStore_NoEnvVarsFallsBackToSQLite proves the base case still
// works: with nothing set, SQLite is the fallback (unaffected by the
// new MySQL branch's insertion point).
func TestInitStore_NoEnvVarsFallsBackToSQLite(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("MYSQL_DSN", "")
	t.Setenv("MONGO_URI", "")
	t.Setenv("DATABASE_PATH", ":memory:")

	s := initStore(context.Background())
	defer s.Close()

	if _, ok := s.(*sqlitestore.SQLiteStore); !ok {
		t.Fatalf("expected no backend env vars to fall back to SQLite, got %T", s)
	}
}
