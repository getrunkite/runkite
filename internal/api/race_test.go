package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/sharanharsoor/runkite/internal/models"
)

// TS-009 regression test: the busy-check and the busy-write for concurrent
// run rejection must be atomic. This test fires genuinely concurrent
// requests (not sequential, unlike TestTS009_ConcurrentRunReject409) --
// a non-atomic check-then-write let multiple runs through under real
// concurrency before this test was added. See internal/state.Store.TryClaimThread.
func TestTS009_GenuinelyConcurrentRequests(t *testing.T) {
	env := newTestEnv(t)
	env.store.CreateThread(context.Background(), &models.Thread{ThreadID: "race-1", Status: models.ThreadStatusIdle})

	const n = 20
	var wg sync.WaitGroup
	statuses := make([]int, n)

	var startWg sync.WaitGroup
	startWg.Add(1)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			startWg.Wait() // release all goroutines at once
			resp, err := postJSON(env.srv.URL+"/threads/race-1/runs", map[string]interface{}{"agent_id": "test"})
			if err != nil {
				t.Logf("request %d error: %v", idx, err)
				return
			}
			statuses[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	startWg.Done() // release the starting gate
	wg.Wait()

	successCount := 0
	for _, code := range statuses {
		switch code {
		case http.StatusOK:
			successCount++
		case http.StatusConflict:
			// expected for all but one
		default:
			t.Logf("unexpected status: %d", code)
		}
	}
	if successCount != 1 {
		t.Errorf("TOCTOU race: %d/%d concurrent runs succeeded on the same thread, want exactly 1", successCount, n)
	}
}
