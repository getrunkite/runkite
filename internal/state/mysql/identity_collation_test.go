package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestMySQLIdentityCollationExact(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"case", "Acme", "acme"},
		{"accent", "Cafe", "Café"},
		{"trailing_space", "Tenant", "Tenant "},
		{"unicode_spelling", "Café", "Cafe\u0301"},
	}
	missing := func(t *testing.T, err error) {
		t.Helper()
		var notFound *state.ErrNotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("distinct identity read another identity's resource: %v", err)
		}
	}
	for _, pair := range pairs {
		t.Run("tenant/"+pair.name, func(t *testing.T) {
			s := newTestStore(t)
			a, b := tenant.WithContext(context.Background(), pair.a), tenant.WithContext(context.Background(), pair.b)
			if err := s.UpsertAgent(a, &models.Agent{AgentID: "shared", Name: "first tenant"}); err != nil {
				t.Fatal(err)
			}
			_, err := s.GetAgent(b, "shared")
			missing(t, err)
			if err := s.UpsertAgent(b, &models.Agent{AgentID: "shared", Name: "second tenant"}); err != nil {
				t.Fatal(err)
			}
			first, err := s.GetAgent(a, "shared")
			if err != nil || first.Name != "first tenant" || first.Version != 1 {
				t.Fatalf("neighbor tenant overwrote first: agent=%+v err=%v", first, err)
			}
			second, err := s.GetAgent(b, "shared")
			if err != nil || second.Name != "second tenant" || second.Version != 1 {
				t.Fatalf("tenant identities collided: agent=%+v err=%v", second, err)
			}
			now := time.Now().UTC()
			if err := s.CreateThread(a, &models.Thread{ThreadID: "private-thread", Status: models.ThreadStatusIdle, Values: map[string]interface{}{"private": "first tenant"}, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			_, err = s.GetThread(b, "private-thread")
			missing(t, err)
		})
		t.Run("resource/"+pair.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := tenant.WithContext(context.Background(), "owner")
			if err := s.UpsertAgent(ctx, &models.Agent{AgentID: pair.a, Name: "first agent"}); err != nil {
				t.Fatal(err)
			}
			_, err := s.GetAgent(ctx, pair.b)
			missing(t, err)
			if err := s.UpsertAgent(ctx, &models.Agent{AgentID: pair.b, Name: "second agent"}); err != nil {
				t.Fatal(err)
			}
			first, err := s.GetAgent(ctx, pair.a)
			if err != nil || first.Name != "first agent" || first.Version != 1 {
				t.Fatalf("neighbor resource overwrote first: agent=%+v err=%v", first, err)
			}
			now := time.Now().UTC()
			for _, id := range []string{pair.a, pair.b} {
				if err := s.CreateThread(ctx, &models.Thread{ThreadID: id, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatalf("distinct thread ID %q collided: %v", id, err)
				}
			}
			for _, key := range []string{pair.a, pair.b} {
				if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"records"}, Key: key, Value: map[string]interface{}{"owner": key}}); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{pair.a, pair.b} {
				item, err := s.GetItem(ctx, []string{"records"}, key, false)
				if err != nil || item.Value["owner"] != key {
					t.Fatalf("distinct item key %q collided: item=%+v err=%v", key, item, err)
				}
			}
		})
	}
}
