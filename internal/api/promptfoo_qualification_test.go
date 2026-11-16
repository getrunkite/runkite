package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// This opt-in qualification exercises native promptfoo CLI, real authenticated
// public HTTP handlers, queue admission, attempt fencing and durable completion.
// The worker is scripted; it does not qualify any LLM or framework runner.
func TestNativePromptfooManagedRuns(t *testing.T) {
	qualifyNativePromptfooManagedRuns(t, false)
}

// This extra source-only profile requires the integrated exact run-observation
// endpoints. Keep the broad baseline above as the published-compatibility gate.
func TestNativePromptfooScopedManagedRuns(t *testing.T) {
	if os.Getenv("RUNKITE_PROMPTFOO_SCOPED_SOURCE") != "1" {
		t.Skip("set RUNKITE_PROMPTFOO_SCOPED_SOURCE=1 on a checkout with scoped run observation")
	}
	qualifyNativePromptfooManagedRuns(t, true)
}

func qualifyNativePromptfooManagedRuns(t *testing.T, scoped bool) {
	t.Helper()
	if os.Getenv("RUNKITE_PROMPTFOO_QUALIFY") != "1" {
		t.Skip("set RUNKITE_PROMPTFOO_QUALIFY=1 after npm ci in examples/promptfoo")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	exampleDirectory := filepath.Join(repository, "examples", "promptfoo")
	if _, err := os.Stat(filepath.Join(exampleDirectory, "node_modules", "promptfoo", "package.json")); err != nil {
		t.Fatal("install pinned examples/promptfoo dependencies first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "pfoo-fixture", Name: "Scripted qualification worker"}); err != nil {
		t.Fatal(err)
	}
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	plane := api.NewServer(store, queue, broker, inprocess.NewCancelBus())
	const key = "synthetic-promptfoo-qualification-key"
	permissions := []string{"read", "write"}
	profile := "tenant-read-write"
	if scoped {
		// Literal vocabulary permits preparing this optional fixture before the
		// source capability is merged. Its real execution must fail on older code.
		permissions = []string{auth.AgentRunPermission("pfoo-fixture"), "agents:pfoo-fixture:observe"}
		profile = "scoped-run-observe"
	}
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{key: {Name: "promptfoo-fixture", TenantID: "default", Permissions: permissions}})
	handler := auth.Middleware(provider, nil, nil, plane.Handler())
	denialProbes := 0
	if scoped {
		for _, denied := range []struct{ method, path string }{
			{http.MethodGet, "/runs"}, {http.MethodGet, "/threads"},
			{http.MethodGet, "/runs/forbidden/stream"},
			{http.MethodPost, "/runs/forbidden/cancel"},
			{http.MethodDelete, "/threads/forbidden"},
			{http.MethodGet, "/admin-api/overview"},
		} {
			r := httptest.NewRequest(denied.method, denied.path, nil)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("scoped profile admitted unrelated route %s %s: %d", denied.method, denied.path, w.Code)
			}
			denialProbes++
		}
	}
	var creates, inlineThreadCreates, reads, waits, pinnedWaits, violations atomic.Int32
	var mu sync.Mutex
	ids := map[string]bool{}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("X-Runkite-Simulation") != "" || strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/internal") {
			violations.Add(1)
			http.Error(w, "fixture public credential boundary violated", http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/threads/") && strings.HasSuffix(r.URL.Path, "/runs"):
			// The public run-create request must create its own fresh thread.
			// No broad POST /threads authority or pre-seeded thread is involved.
			raw, readErr := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request models.RunCreate
			parts := strings.Split(r.URL.Path, "/")
			if readErr != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &request) != nil || request.IfNotExists != "create" || len(parts) != 4 || parts[2] == "" {
				violations.Add(1)
				http.Error(w, "fixture requires inline thread creation", http.StatusBadRequest)
				return
			}
			_, threadErr := store.GetThread(ctx, parts[2])
			var notFound *state.ErrNotFound
			if !errors.As(threadErr, &notFound) {
				violations.Add(1)
				http.Error(w, "fixture thread must not exist before run creation", http.StatusConflict)
				return
			}
			inlineThreadCreates.Add(1)
			creates.Add(1)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/runs/"):
			if strings.HasSuffix(r.URL.Path, "/wait") {
				waits.Add(1)
			} else {
				reads.Add(1)
			}
		default:
			violations.Add(1)
			http.Error(w, "fixture route boundary violated", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/wait") {
			expected := r.Header.Get("Runkite-Expected-Run-Reference")
			if expected != "" && w.Header().Get("Runkite-Run-Reference") == expected {
				pinnedWaits.Add(1)
			}
		}
	})}}
	server.Start()
	defer server.Close()
	workerError := make(chan error, 1)
	go func() {
		for completed := 0; completed < 4; {
			assignment, err := queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil {
				workerError <- fmt.Errorf("dequeue: %v", err)
				return
			}
			if assignment == nil {
				continue
			}
			var input struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(assignment.Input, &input); err != nil || len(input.Messages) != 1 {
				workerError <- fmt.Errorf("invalid assignment input")
				return
			}
			mu.Lock()
			ids[assignment.RunID] = true
			mu.Unlock()
			accepted, err := plane.BeginRunAttempt(ctx, assignment.RunID, assignment.Generation)
			if err != nil || !accepted {
				workerError <- fmt.Errorf("attempt: %v", err)
				return
			}
			values := map[string]interface{}{"messages": []map[string]interface{}{{"role": "assistant", "content": "scripted:" + input.Messages[0].Content}}}
			accepted, err = plane.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: assignment.RunID, Generation: assignment.Generation, Status: models.RunStatusSuccess, Values: values})
			if err != nil || !accepted {
				workerError <- fmt.Errorf("completion: %v", err)
				return
			}
			completed++
		}
		workerError <- nil
	}()
	command := exec.CommandContext(ctx, "node", "qualify-native.mjs")
	command.Dir = exampleDirectory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "RUNKITE_URL=" + server.URL, "RUNKITE_API_KEY=" + key}
	output, err := command.CombinedOutput()
	if strings.Contains(string(output), key) {
		t.Fatal("credential appeared in qualification output")
	}
	if err != nil {
		t.Fatalf("native CLI qualification: %v\n%s", err, output)
	}
	if err := <-workerError; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	distinct := len(ids)
	mu.Unlock()
	if creates.Load() != 4 || reads.Load() != 4 || waits.Load() != 4 || violations.Load() != 0 || distinct != 4 {
		t.Fatalf("unexpected public traffic: creates=%d reads=%d waits=%d violations=%d distinct=%d", creates.Load(), reads.Load(), waits.Load(), violations.Load(), distinct)
	}
	threads, err := store.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: 10})
	if err != nil || len(threads) != 4 || inlineThreadCreates.Load() != 4 {
		t.Fatalf("expected four fresh threads created inline by run admission: threads=%d inline=%d error=%v", len(threads), inlineThreadCreates.Load(), err)
	}
	var native struct {
		Node                 string `json:"node"`
		Promptfoo            string `json:"promptfoo"`
		RunReferenceAcquired bool   `json:"run_reference_acquired"`
	}
	if err := json.Unmarshal(output, &native); err != nil {
		t.Fatal("invalid native summary")
	}
	if scoped && (!native.RunReferenceAcquired || pinnedWaits.Load() != 4) {
		t.Fatalf("scoped source requires four durable reference pins and matching waits: acquired=%v waits=%d", native.RunReferenceAcquired, pinnedWaits.Load())
	}
	if reportPath := os.Getenv("RUNKITE_PROMPTFOO_REPORT"); reportPath != "" {
		// Retain only allowlisted synthetic summary fields, never receipts,
		// evaluation prompts, outputs, credentials or process logs.
		report, err := json.MarshalIndent(map[string]interface{}{"passed": true, "node": native.Node, "promptfoo": native.Promptfoo, "managed_runs": distinct, "managed_threads": len(threads), "inline_thread_creates": inlineThreadCreates.Load(), "http_creates": creates.Load(), "http_reads": reads.Load(), "http_waits": waits.Load(), "reference_pinned_waits": pinnedWaits.Load(), "run_reference_acquired": native.RunReferenceAcquired, "permission_profile": profile, "scoped_denial_probes": denialProbes, "boundary_violations": violations.Load(), "model_calls": 0, "worker": "scripted", "store": "sqlite", "transport": "inprocess"}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reportPath, append(report, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(strings.TrimSpace(string(output)))
}
