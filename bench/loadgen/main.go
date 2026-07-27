// loadgen is runkite's internal load generator, for backend-comparison
// runs and any future "did this change regress latency/memory" check.
// Deliberately not a general-purpose load testing tool (that's what
// Locust is for -- see bench/locust/ for resource-constrained scenarios)
// -- this is small, Go-native (no separate runtime/dependency to
// install), and specific to runkite's own create-thread-and-run-and-wait
// cycle. Also works against other LangGraph SDK-compatible servers (see
// -wait-path) for quick ad-hoc comparisons outside Locust.
//
// Usage:
//
//	go run ./bench/loadgen -url http://localhost:2026 -agent-id echo_agent -concurrency 150 -duration 30s
//	go run ./bench/loadgen -url http://localhost:2026 -agent-id echo_agent -concurrency 150 -duration 30s -pid 12345
//
// With -pid set, samples the given process's RSS every second for the
// run's duration and reports min/max/delta alongside latency -- the
// same "does memory plateau or grow unbounded" signal used in this
// project's manual smoke-test verification, just automated and reusable
// instead of a one-off /tmp script.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latency time.Duration
	err     error
}

func main() {
	var (
		baseURL     = flag.String("url", "http://localhost:2026", "control plane base URL")
		agentID     = flag.String("agent-id", "echo_agent", "agent_id to dispatch runs to")
		concurrency = flag.Int("concurrency", 50, "number of concurrent workers")
		duration    = flag.Duration("duration", 30*time.Second, "how long to run")
		pid         = flag.Int("pid", 0, "optional PID to sample RSS from during the run (0 = skip)")
		waitTimeout = flag.Duration("wait-timeout", 30*time.Second, "per-run HTTP client timeout")
		waitPath    = flag.String("wait-path", "wait", "run-completion blocking endpoint segment: \"wait\" (runkite) or \"join\" (some other LangGraph SDK-compatible servers) -- both block until terminal, but return different response shapes, so status is always re-checked via a plain GET afterward for a uniform comparison across servers")
	)
	flag.Parse()

	client := &http.Client{Timeout: *waitTimeout}
	var successCount, errorCount int64
	var results []result
	var resultsMu sync.Mutex

	var rssSamples []int64
	stopRSS := make(chan struct{})
	var rssWG sync.WaitGroup
	if *pid > 0 {
		rssWG.Add(1)
		go func() {
			defer rssWG.Done()
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopRSS:
					return
				case <-ticker.C:
					if rss, err := sampleRSS(*pid); err == nil {
						resultsMu.Lock()
						rssSamples = append(rssSamples, rss)
						resultsMu.Unlock()
					}
				}
			}
		}()
	}

	fmt.Printf("loadgen: url=%s agent_id=%s concurrency=%d duration=%s\n", *baseURL, *agentID, *concurrency, *duration)

	deadline := time.Now().Add(*duration)
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			n := 0
			for time.Now().Before(deadline) {
				t0 := time.Now()
				err := oneCycle(client, *baseURL, *agentID, *waitPath, worker, n)
				lat := time.Since(t0)
				n++

				resultsMu.Lock()
				results = append(results, result{latency: lat, err: err})
				resultsMu.Unlock()

				if err == nil {
					atomic.AddInt64(&successCount, 1)
				} else {
					atomic.AddInt64(&errorCount, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	close(stopRSS)
	rssWG.Wait()

	report(results, successCount, errorCount, rssSamples)
}

func report(results []result, successCount, errorCount int64, rssSamples []int64) {
	if len(results) == 0 {
		fmt.Println("no requests completed")
		return
	}
	latencies := make([]time.Duration, len(results))
	for i, r := range results {
		latencies[i] = r.latency
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	pct := func(p float64) time.Duration {
		idx := int(float64(len(latencies)) * p)
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}

	total := successCount + errorCount
	fmt.Printf("\n--- results ---\n")
	fmt.Printf("total=%d success=%d errors=%d error_rate=%.2f%%\n", total, successCount, errorCount, 100*float64(errorCount)/float64(total))
	fmt.Printf("p50=%s p90=%s p99=%s max=%s\n", pct(0.50), pct(0.90), pct(0.99), latencies[len(latencies)-1])

	if len(rssSamples) > 0 {
		min, max := rssSamples[0], rssSamples[0]
		for _, v := range rssSamples {
			if v < min {
				min = v
			}
			if v > max {
				max = v
			}
		}
		fmt.Printf("rss: samples=%d min=%dKB max=%dKB delta=%dKB\n", len(rssSamples), min, max, max-min)
	}

	if errorCount > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Printf("first error: %v\n", r.err)
				break
			}
		}
	}
}

// oneCycle: create a thread, create a run on it, block until it reaches a
// terminal state via the server's own blocking endpoint (no local polling
// loop, no artificial delay), then re-check status via a plain GET.
//
// The blocking call's own response shape differs between servers (runkite's
// /wait returns {"run": {...}, "values": {...}}; some other LangGraph
// SDK-compatible servers' /join returns just the raw output values, no
// status field) -- rather than parsing two different shapes, its response
// is used only to unblock, and a follow-up
// GET /threads/{id}/runs/{run_id} (identical response shape on both
// servers: both expose a top-level "status" field) is the single source of
// truth for success/failure, so the comparison is apples-to-apples.
func oneCycle(client *http.Client, baseURL, agentID, waitPath string, worker, n int) error {
	threadID := fmt.Sprintf("loadgen-%d-%d-%d", time.Now().UnixNano(), worker, n)

	body, _ := json.Marshal(map[string]interface{}{
		// assistant_id, not agent_id: runkite accepts assistant_id as an
		// alias for agent_id (SDK compat), but the LangGraph SDK's own
		// convention only ever uses assistant_id -- using this field name
		// lets the exact same request body run unmodified against any
		// LangGraph SDK-compatible server.
		"assistant_id": agentID,
		"input":        map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "ping"}}},
	})
	resp, err := client.Post(baseURL+"/threads/"+threadID+"/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("create run: status %d body=%s", resp.StatusCode, respBody)
	}
	var run struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(respBody, &run); err != nil {
		return fmt.Errorf("parse run: %w", err)
	}

	blockResp, err := client.Get(baseURL + "/threads/" + threadID + "/runs/" + run.RunID + "/" + waitPath)
	if err != nil {
		return fmt.Errorf("%s run: %w", waitPath, err)
	}
	io.Copy(io.Discard, blockResp.Body)
	blockResp.Body.Close()

	statusResp, err := client.Get(baseURL + "/threads/" + threadID + "/runs/" + run.RunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	defer statusResp.Body.Close()
	b, _ := io.ReadAll(statusResp.Body)
	var got struct {
		Status string `json:"status"`
	}
	json.Unmarshal(b, &got)
	if got.Status != "success" {
		return fmt.Errorf("run terminal status=%q body=%s", got.Status, b)
	}
	return nil
}

// sampleRSS reads a process's resident set size in KB. Uses `ps` rather
// than /proc (this project also needs to run on macOS for local dev,
// where /proc doesn't exist) -- fine for a once-per-second sample, not
// meant for high-frequency profiling (that's pprof's job, see
// RUNKITE_PPROF in cmd/serve.go).
func sampleRSS(pid int) (int64, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						return strconv.ParseInt(fields[1], 10, 64)
					}
				}
			}
		}
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}
