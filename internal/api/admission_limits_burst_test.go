package api_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
)

// TestCreateRun_AdmissionConcurrentBurst proves max_concurrent holds under
// a real parallel burst (the failure mode of a check-then-create TOCTOU).
func TestCreateRun_AdmissionConcurrentBurst(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "burst-agent")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{AgentConcurrent: 1})

	const n = 20
	var okCount, limited atomic.Int32
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			resp, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "burst-agent"})
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			code := resp.StatusCode
			resp.Body.Close()
			switch code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			default:
				t.Errorf("unexpected status %d", code)
			}
		}()
	}
	close(start)
	wg.Wait()

	if okCount.Load() != 1 {
		t.Fatalf("admitted %d runs under AgentConcurrent=1 (want exactly 1); 429s=%d", okCount.Load(), limited.Load())
	}
	if limited.Load() != n-1 {
		t.Fatalf("got %d 429s, want %d", limited.Load(), n-1)
	}
}
