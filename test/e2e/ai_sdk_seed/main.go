// Test fixture registration for tenant-scoped managed runner integration tests.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
)

func seed() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: e2e_seed CONFIG TENANT_IDS")
	}
	cfg, err := config.LoadLangGraphJSON(os.Args[1])
	if err != nil {
		return err
	}
	if cfg.RunnerKind != "typescript-ai-sdk" {
		return fmt.Errorf("managed AI SDK config routes to %q", cfg.RunnerKind)
	}
	ctx := context.Background()
	var store state.Store
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		store, err = postgres.New(ctx, dsn)
	} else {
		store, err = sqlite.New(os.Getenv("DATABASE_PATH"))
	}
	if err != nil {
		return err
	}
	defer store.Close() //nolint:errcheck // fixture cleanup after registration
	for _, id := range strings.Split(os.Args[2], ",") {
		if id == "" {
			return fmt.Errorf("empty test tenant")
		}
		for graphID := range cfg.Graphs {
			metadata := map[string]interface{}{"runner_kind": cfg.RunnerKind}
			if tools, ok := cfg.AllowedTools[graphID]; ok {
				metadata["allowed_tools"] = tools
				metadata["allowed_tools_set"] = true
			}
			if err := store.UpsertAgent(tenant.WithContext(ctx, id), &models.Agent{
				AgentID: graphID, Name: graphID,
				Metadata: metadata, Capabilities: map[string]interface{}{},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	if err := seed(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
