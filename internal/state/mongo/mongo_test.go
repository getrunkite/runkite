package mongo_test

import (
	"context"
	"os"
	"testing"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/conformance"
	runkiteMongo "github.com/getrunkite/runkite/internal/state/mongo"
)

func TestMongoStore(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set — skipping MongoDB conformance tests")
	}
	const dbName = "runkite_test"

	ctx := context.Background()
	setupStore, err := runkiteMongo.New(ctx, uri, dbName)
	if err != nil {
		t.Fatalf("connect to mongo: %v", err)
	}
	if err := setupStore.Init(ctx); err != nil {
		t.Fatalf("init mongo store: %v", err)
	}
	setupStore.Close()

	conformance.RunStoreSuite(t, func(t *testing.T) state.Store {
		ctx := context.Background()
		s, err := runkiteMongo.New(ctx, uri, dbName)
		if err != nil {
			t.Fatalf("connect to mongo: %v", err)
		}
		// Clean slate for each subtest
		if err := s.TruncateAll(ctx); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
