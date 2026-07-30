package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	mysqlstore "github.com/sharanharsoor/runkite/internal/state/mysql"
	pgstore "github.com/sharanharsoor/runkite/internal/state/postgres"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
)

func cmdDBUpgrade(args []string) {
	fs := flag.NewFlagSet("db upgrade", flag.ExitOnError)
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()

	postgresDSN := os.Getenv("POSTGRES_DSN")
	if postgresDSN != "" {
		pg, err := pgstore.New(ctx, postgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres", "error", err)
			os.Exit(1)
		}
		defer pg.Close()
		if err := pg.Init(ctx); err != nil {
			slog.Error("failed to initialize postgres", "error", err)
			os.Exit(1)
		}
		fmt.Println("Database initialized successfully (postgres)")
		return
	}

	if mysqlDSN := os.Getenv("MYSQL_DSN"); mysqlDSN != "" {
		my, err := mysqlstore.New(ctx, mysqlDSN)
		if err != nil {
			slog.Error("failed to connect to mysql", "error", err)
			os.Exit(1)
		}
		defer my.Close()
		if err := my.Init(ctx); err != nil {
			slog.Error("failed to initialize mysql", "error", err)
			os.Exit(1)
		}
		fmt.Println("Database initialized successfully (mysql)")
		return
	}

	dbPath := envOrDefault("DATABASE_PATH", "./runkite.db")
	sq, err := sqlitestore.New(dbPath)
	if err != nil {
		slog.Error("failed to create sqlite store", "error", err)
		os.Exit(1)
	}
	defer sq.Close()
	if err := sq.Init(ctx); err != nil {
		slog.Error("failed to initialize sqlite", "error", err)
		os.Exit(1)
	}
	fmt.Println("Database initialized successfully (sqlite)")
}

// cmdDBDowngrade exists so the CLI surface matches the documented command
// set, but there is currently no real migration to roll back: the schema is
// a single idempotent `CREATE TABLE IF NOT EXISTS` script (see Init in
// internal/state/sqlite and internal/state/postgres), not a sequence of
// versioned up/down migrations. Silently no-op'ing or pretending to succeed
// would be worse than being honest that this isn't implemented yet -- a
// real "downgrade" needs a versioned migration framework (numbered
// migrations with paired up/down steps and a schema_version table) before
// this command can do anything meaningful.
func cmdDBDowngrade(args []string) {
	fs := flag.NewFlagSet("db downgrade", flag.ExitOnError)
	fs.Parse(args)

	fmt.Fprintln(os.Stderr, "runkite db downgrade: not supported yet.")
	fmt.Fprintln(os.Stderr, "The schema is managed as a single idempotent migration (CREATE TABLE IF")
	fmt.Fprintln(os.Stderr, "NOT EXISTS), not versioned up/down migrations, so there is nothing to roll")
	fmt.Fprintln(os.Stderr, "back to. If you need a clean slate, use `runkite db reset` (drops and")
	fmt.Fprintln(os.Stderr, "recreates all tables -- this deletes existing data).")
	os.Exit(1)
}

func cmdDBReset(args []string) {
	fs := flag.NewFlagSet("db reset", flag.ExitOnError)
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()

	postgresDSN := os.Getenv("POSTGRES_DSN")
	if postgresDSN != "" {
		pg, err := pgstore.New(ctx, postgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres", "error", err)
			os.Exit(1)
		}
		defer pg.Close()
		if err := pg.TruncateAll(ctx); err != nil {
			slog.Error("failed to truncate postgres", "error", err)
			os.Exit(1)
		}
		if err := pg.Init(ctx); err != nil {
			slog.Error("failed to reinitialize postgres", "error", err)
			os.Exit(1)
		}
		fmt.Println("Database reset successfully (postgres)")
		return
	}

	if mysqlDSN := os.Getenv("MYSQL_DSN"); mysqlDSN != "" {
		my, err := mysqlstore.New(ctx, mysqlDSN)
		if err != nil {
			slog.Error("failed to connect to mysql", "error", err)
			os.Exit(1)
		}
		defer my.Close()
		if err := my.TruncateAll(ctx); err != nil {
			slog.Error("failed to truncate mysql", "error", err)
			os.Exit(1)
		}
		if err := my.Init(ctx); err != nil {
			slog.Error("failed to reinitialize mysql", "error", err)
			os.Exit(1)
		}
		fmt.Println("Database reset successfully (mysql)")
		return
	}

	// SQLite: delete the file and recreate
	dbPath := envOrDefault("DATABASE_PATH", "./runkite.db")
	if dbPath != "" && dbPath != ":memory:" {
		os.Remove(dbPath)
	}
	sq, err := sqlitestore.New(dbPath)
	if err != nil {
		slog.Error("failed to create sqlite store", "error", err)
		os.Exit(1)
	}
	defer sq.Close()
	if err := sq.Init(ctx); err != nil {
		slog.Error("failed to initialize sqlite", "error", err)
		os.Exit(1)
	}
	fmt.Println("Database reset successfully (sqlite)")
}
