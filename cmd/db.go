package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	mongostore "github.com/getrunkite/runkite/internal/state/mongo"
	mysqlstore "github.com/getrunkite/runkite/internal/state/mysql"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
)

// dbBackend is one opened state store plus helpers used by db CLI commands.
// TruncateAll is not on state.Store (test/ops helper), so each backend
// wires its own resetFn.
type dbBackend struct {
	name    string
	store   state.Store
	close   func()
	resetFn func(context.Context) error
}

func openDBBackend(ctx context.Context) (*dbBackend, error) {
	if postgresDSN := os.Getenv("POSTGRES_DSN"); postgresDSN != "" {
		pg, err := pgstore.New(ctx, postgresDSN)
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		return &dbBackend{
			name:  "postgres",
			store: pg,
			close: func() { _ = pg.Close() },
			resetFn: func(ctx context.Context) error {
				if err := pg.TruncateAll(ctx); err != nil {
					return err
				}
				return pg.Init(ctx)
			},
		}, nil
	}
	if mysqlDSN := os.Getenv("MYSQL_DSN"); mysqlDSN != "" {
		my, err := mysqlstore.New(ctx, mysqlDSN)
		if err != nil {
			return nil, fmt.Errorf("mysql: %w", err)
		}
		return &dbBackend{
			name:  "mysql",
			store: my,
			close: func() { _ = my.Close() },
			resetFn: func(ctx context.Context) error {
				if err := my.TruncateAll(ctx); err != nil {
					return err
				}
				return my.Init(ctx)
			},
		}, nil
	}
	if mongoURI := os.Getenv("MONGO_URI"); mongoURI != "" {
		dbName := envOrDefault("MONGO_DB", "runkite")
		mg, err := mongostore.New(ctx, mongoURI, dbName)
		if err != nil {
			return nil, fmt.Errorf("mongodb: %w", err)
		}
		return &dbBackend{
			name:  "mongodb",
			store: mg,
			close: func() { _ = mg.Close() },
			resetFn: func(ctx context.Context) error {
				if err := mg.TruncateAll(ctx); err != nil {
					return err
				}
				return mg.Init(ctx)
			},
		}, nil
	}

	dbPath := envOrDefault("DATABASE_PATH", "./runkite.db")
	sq, err := sqlitestore.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	b := &dbBackend{
		name:  "sqlite",
		store: sq,
		close: func() { _ = sq.Close() },
	}
	b.resetFn = func(ctx context.Context) error {
		_ = sq.Close()
		if dbPath != "" && dbPath != ":memory:" {
			if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
				slog.Warn("failed to remove existing sqlite db before reset", "path", dbPath, "error", err)
			}
		}
		fresh, err := sqlitestore.New(dbPath)
		if err != nil {
			return err
		}
		b.store = fresh
		b.close = func() { _ = fresh.Close() }
		sq = fresh
		return fresh.Init(ctx)
	}
	return b, nil
}

func cmdDBUpgrade(args []string) {
	fs := flag.NewFlagSet("db upgrade", flag.ExitOnError)
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()
	b, err := openDBBackend(ctx)
	if err != nil {
		slog.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer b.close()
	if err := b.store.Init(ctx); err != nil {
		slog.Error("failed to apply migrations", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Database upgraded successfully (%s)\n", b.name)
}

func cmdDBDowngrade(args []string) {
	fs := flag.NewFlagSet("db downgrade", flag.ExitOnError)
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()
	b, err := openDBBackend(ctx)
	if err != nil {
		slog.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer b.close()
	// Baseline (currently the only migration) Down drops every application
	// table/collection -- same class of risk as db reset. Future v2+ Downs
	// are smaller; until then, say so loudly before executing.
	fmt.Fprintln(os.Stderr, "warning: db downgrade runs the previous migration's Down step.")
	fmt.Fprintln(os.Stderr, "While only baseline (v1) exists, that drops all application tables/collections.")
	if err := b.store.Downgrade(ctx); err != nil {
		if errors.Is(err, migrate.ErrNoMigration) {
			fmt.Fprintln(os.Stderr, "runkite db downgrade: nothing to roll back (schema version is 0).")
			os.Exit(1)
		}
		slog.Error("failed to downgrade", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Rolled back one migration (%s)\n", b.name)
}

func cmdDBReset(args []string) {
	fs := flag.NewFlagSet("db reset", flag.ExitOnError)
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()
	b, err := openDBBackend(ctx)
	if err != nil {
		slog.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer b.close()
	if err := b.resetFn(ctx); err != nil {
		slog.Error("failed to reset", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Database reset successfully (%s)\n", b.name)
}
