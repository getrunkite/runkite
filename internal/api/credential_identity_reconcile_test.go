package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func installCredentialFixtureWorkload(t *testing.T, env *actionAPIEnv, owner *transport.WorkloadIdentity) {
	t.Helper()
	ctx := transport.WithWorkloadIdentity(env.ctx, owner)
	if ok, err := env.store.BeginRunAttempt(ctx, "run", originalRunIdentity(t, env.store, "run"), 3); err != nil || !ok {
		t.Fatalf("bind workload attempt: accepted=%v error=%v", ok, err)
	}
	live, err := env.queue.LookupInflight(env.ctx, "run")
	if err != nil || live == nil {
		t.Fatal("missing fixture assignment")
	}
	if accepted, err := env.queue.Ack(env.ctx, "run", originalRunIdentity(t, env.store,

		"run"), live.Generation); err != nil || !accepted {
		t.Fatalf("replace fixture assignment: accepted=%v error=%v", accepted, err)
	}
	live.Generation, live.WorkloadOwner = 3, owner
	if err := env.queue.Enqueue(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := env.queue.Dequeue(ctx, live.RunnerKind, time.Second); err != nil {
		t.Fatal(err)
	}
	env.binding.Generation, env.binding.WorkloadIdentity = 3, owner
	env.ctx = ctx
}

func TestCredentialReconciliationChecksLivenessWithoutWorkerCertificate(t *testing.T) {
	for _, scenario := range []string{"active", "cancelled", "reclaimed", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			env, provider := newVaultActionFixture(t, "policy")
			owner := &transport.WorkloadIdentity{URI: "spiffe://fixture.test/worker", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(time.Minute)}
			if scenario == "expired" {
				owner.NotAfter = time.Now().Add(3 * time.Second)
			}
			installCredentialFixtureWorkload(t, env, owner)
			entered, release := make(chan struct{}), make(chan struct{})
			releaseConsumer := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseConsumer)
			env.setTool(func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[]}}`))
			})
			requestCtx, cancel := context.WithTimeout(env.ctx, 8*time.Second)
			t.Cleanup(cancel)
			r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(recordedToolCall)).WithContext(requestCtx)
			r.SetPathValue("name", "fixture")
			attachConnectorSession(t, env.s, env.binding, "fixture", r)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.s.handleProxyMCPRequest(response, r)
			}()
			t.Cleanup(func() {
				releaseConsumer()
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("credential consumer did not stop")
				}
			})
			select {
			case <-entered:
			case <-done:
				t.Fatalf("consumer did not start: code=%d body=%s", response.Code, response.Body.String())
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for protected consumer")
			}
			rows, err := env.s.credentialManager.Search(env.ctx, "acme", 10)
			if err != nil || len(rows) != 1 || rows[0].State != credentials.StateActive || provider.issued.Load() != 1 {
				t.Fatalf("missing active credential: count=%d error=%v", len(rows), err)
			}
			row := rows[0]
			background := tenant.SystemContext(context.Background())
			if _, err := env.s.credentialManager.Renew(background, row.Owner, row.ID); !errors.Is(err, credentials.ErrStaleOwner) || provider.renewed.Load() != 0 {
				t.Fatal("a background context acquired worker renewal authority")
			}
			switch scenario {
			case "cancelled":
				if err := env.queue.Cancel(env.ctx, "run", originalRunIdentity(t, env.store,

					"run")); err != nil {
					t.Fatal(err)
				}
			case "reclaimed":
				replacement := *owner
				replacement.KeySHA256 = strings.Repeat("b", 64)
				newCtx := transport.WithWorkloadIdentity(env.ctx, &replacement)
				if ok, err := env.store.BeginRunAttempt(newCtx, "run", originalRunIdentity(t, env.store, "run"), 4); err != nil || !ok {
					t.Fatalf("reclaim durable attempt: accepted=%v error=%v", ok, err)
				}
				live, err := env.queue.LookupInflight(env.ctx, "run")
				if err != nil || live == nil {
					t.Fatal("missing original assignment during reclaim")
				}
				if accepted, err := env.queue.Ack(env.ctx, "run", originalRunIdentity(t, env.store,

					"run"), 3); err != nil || !accepted {
					t.Fatal("could not retire original assignment")
				}
				live.Generation, live.WorkloadOwner = 4, nil
				if err := env.queue.Enqueue(newCtx, live); err != nil {
					t.Fatal(err)
				}
				if _, err := env.queue.Dequeue(newCtx, live.RunnerKind, time.Second); err != nil {
					t.Fatal(err)
				}
			case "expired":
				timer := time.NewTimer(max(0, time.Until(owner.NotAfter)+time.Millisecond))
				defer timer.Stop()
				<-timer.C
			}
			_, err = env.s.credentialManager.Reconcile(background, 100)
			if err != nil {
				t.Fatalf("background reconciliation failed: %v", err)
			}
			current, err := env.s.credentialManager.Get(env.ctx, "acme", row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "active" {
				if provider.revoked.Load() != 0 || current.State != credentials.StateActive {
					t.Fatal("background liveness check revoked a valid mTLS-owned credential")
				}
				if _, err := env.s.credentialManager.Renew(background, row.Owner, row.ID); !errors.Is(err, credentials.ErrStaleOwner) || provider.renewed.Load() != 0 {
					t.Fatal("reconciliation authority leaked into ordinary renewal")
				}
			} else if provider.revoked.Load() != 1 || current.State != credentials.StateRevoked {
				t.Fatal("a cancelled, reclaimed or expired owner retained its credential")
			}
			releaseConsumer()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("consumer did not finish after release")
			}
			if provider.issued.Load() != 1 || env.effects.Load() != 1 || provider.revoked.Load() != 1 {
				t.Fatal("reconciliation repeated issuance, execution or cleanup")
			}
			if auth.RunBindingFromContext(env.ctx).Generation != 3 {
				t.Fatal("maintenance changed caller generation")
			}
		})
	}
}
