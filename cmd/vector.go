package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/vectorstore"
	"github.com/getrunkite/runkite/internal/vectorstore/pgvector"
	pineconestore "github.com/getrunkite/runkite/internal/vectorstore/pinecone"
	"github.com/getrunkite/runkite/internal/vectorstore/qdrant"
	weaviatestore "github.com/getrunkite/runkite/internal/vectorstore/weaviate"
)

// vectorDowngrader is implemented by backends with a real Down step
// (pgvector today). Cloud create-if-missing backends have nothing to roll back.
type vectorDowngrader interface {
	Downgrade(ctx context.Context) error
}

func openVectorBackend(ctx context.Context, configPath string) (name string, vs vectorstore.VectorStore, err error) {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return "", nil, fmt.Errorf("no langgraph.json found (pass --config or set LANGGRAPH_CONFIG)")
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil {
		return "", nil, err
	}
	if cfg.VectorStore == nil {
		return "", nil, fmt.Errorf("%s has no vector_store section", paths[0])
	}
	dims := cfg.VectorStore.Dimensions
	if dims <= 0 {
		dims = defaultVectorDimensions
	}

	switch cfg.VectorStore.Type {
	case "pgvector":
		dsn := os.Getenv("POSTGRES_DSN")
		if dsn == "" {
			return "", nil, fmt.Errorf("vector_store.type=pgvector requires POSTGRES_DSN")
		}
		s, err := pgvector.New(ctx, dsn, dims)
		if err != nil {
			return "", nil, err
		}
		return "pgvector", s, nil

	case "qdrant":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("QDRANT_URL")
		}
		if url == "" {
			return "", nil, fmt.Errorf("vector_store.type=qdrant requires vector_store.url or QDRANT_URL")
		}
		s, err := qdrant.New(url, cfg.VectorStore.Collection, dims)
		if err != nil {
			return "", nil, err
		}
		return "qdrant", s, nil

	case "weaviate":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("WEAVIATE_URL")
		}
		if url == "" {
			return "", nil, fmt.Errorf("vector_store.type=weaviate requires vector_store.url or WEAVIATE_URL")
		}
		s, err := weaviatestore.New(url, cfg.VectorStore.Class, dims)
		if err != nil {
			return "", nil, err
		}
		return "weaviate", s, nil

	case "pinecone":
		url := cfg.VectorStore.URL
		if url == "" {
			url = os.Getenv("PINECONE_URL")
		}
		if url == "" {
			url = "https://api.pinecone.io"
		}
		apiKey := cfg.VectorStore.APIKey
		if apiKey == "" {
			apiKey = os.Getenv("PINECONE_API_KEY")
		}
		if apiKey == "" && url == "https://api.pinecone.io" {
			return "", nil, fmt.Errorf("vector_store.type=pinecone requires api_key / PINECONE_API_KEY for the real service")
		}
		s, err := pineconestore.New(url, apiKey, cfg.VectorStore.Index, dims, cfg.VectorStore.Cloud, cfg.VectorStore.Region)
		if err != nil {
			return "", nil, err
		}
		return "pinecone", s, nil

	default:
		return "", nil, fmt.Errorf("unsupported vector_store.type %q", cfg.VectorStore.Type)
	}
}

func cmdVectorUpgrade(args []string) {
	fs := flag.NewFlagSet("vector upgrade", flag.ExitOnError)
	configPath := fs.String("config", "", "path to langgraph.json")
	fs.StringVar(configPath, "c", "", "path to langgraph.json (shorthand)")
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()
	name, vs, err := openVectorBackend(ctx, resolveConfig(*configPath))
	if err != nil {
		slog.Error("failed to open vector store", "error", err)
		os.Exit(1)
	}
	defer vs.Close()
	if err := vs.Init(ctx); err != nil {
		slog.Error("failed to apply vector migrations", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Vector store upgraded successfully (%s)\n", name)
}

func cmdVectorDowngrade(args []string) {
	fs := flag.NewFlagSet("vector downgrade", flag.ExitOnError)
	configPath := fs.String("config", "", "path to langgraph.json")
	fs.StringVar(configPath, "c", "", "path to langgraph.json (shorthand)")
	fs.Parse(args)

	setupLogging()
	ctx := context.Background()
	name, vs, err := openVectorBackend(ctx, resolveConfig(*configPath))
	if err != nil {
		slog.Error("failed to open vector store", "error", err)
		os.Exit(1)
	}
	defer vs.Close()

	d, ok := vs.(vectorDowngrader)
	if !ok {
		fmt.Fprintf(os.Stderr, "runkite vector downgrade: %s has no versioned Down step (create-if-missing Init only)\n", name)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "warning: vector downgrade runs the previous migration's Down step.")
	fmt.Fprintln(os.Stderr, "While only baseline (v1) exists for pgvector, that drops vector_items.")
	if err := d.Downgrade(ctx); err != nil {
		if errors.Is(err, migrate.ErrNoMigration) {
			fmt.Fprintln(os.Stderr, "runkite vector downgrade: nothing to roll back (schema version is 0).")
			os.Exit(1)
		}
		slog.Error("failed to downgrade vector store", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Rolled back one vector migration (%s)\n", name)
}
