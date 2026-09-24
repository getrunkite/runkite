package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
)

// simPollInterval is the GET /runs/{id} wait between polls. 1s is courtesy
// so a case that hits a real model under a multi-minute timeout does not
// hammer the plane. Tests shrink this.
var simPollInterval = time.Second

func cmdSim(args []string) {
	os.Exit(runSim(os.Stdout, os.Stderr, args))
}

func runSim(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: runkite sim -f fixtures.yaml [options]

Replay fixture YAML against a live control plane. Each case creates a
fresh thread and a run with X-Runkite-Simulation so FinOps holds and
UTC-day run caps skip; policy, kill, rate limits, and concurrent
admission still apply. Not a sandbox: allow cases hit real connectors.

Flags:
  -f, --file      fixture YAML (required)
  --url           control plane URL (env RUNKITE_URL; default http://127.0.0.1:2026)
  --api-key       Bearer token (env RUNKITE_API_KEY); needs admin when auth is on
  --timeout       per-case deadline (default 2m)
  --tenant        X-Runkite-Tenant-Id (env RUNKITE_TENANT; else file-level tenant)
`)
	}
	file := fs.String("file", "", "path to fixture YAML")
	fs.StringVar(file, "f", "", "path to fixture YAML")
	rawURL := fs.String("url", "", "control plane URL")
	apiKey := fs.String("api-key", "", "Bearer token")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-case deadline")
	tenantFlag := fs.String("tenant", "", "tenant id header")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "sim: -f is required")
		fs.Usage()
		return 1
	}
	base := strings.TrimSpace(*rawURL)
	if base == "" {
		base = os.Getenv("RUNKITE_URL")
	}
	if base == "" {
		base = "http://127.0.0.1:2026"
	}
	base = strings.TrimRight(base, "/")
	key := *apiKey
	if key == "" {
		key = os.Getenv("RUNKITE_API_KEY")
	}
	tenant := *tenantFlag
	if tenant == "" {
		tenant = os.Getenv("RUNKITE_TENANT")
	}

	doc, err := parseSimFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "sim: %v\n", err)
		return 1
	}
	if tenant == "" {
		tenant = doc.Tenant
	}

	cli := &simClient{
		base:   base,
		apiKey: key,
		tenant: tenant,
		http:   &http.Client{},
		poll:   simPollInterval,
	}

	passed, failed := 0, 0
	for _, c := range doc.Cases {
		start := time.Now()
		runID, err := cli.runCase(c, doc.Agent, *timeout)
		elapsed := time.Since(start)
		if err != nil {
			failed++
			id := runID
			if id == "" {
				id = "-"
			}
			fmt.Fprintf(stdout, "FAIL  %s   run_id=%s  %s\n", c.ID, id, err.Error())
			continue
		}
		passed++
		fmt.Fprintf(stdout, "PASS  %s   run_id=%s  %s\n", c.ID, runID, formatSimDur(elapsed))
	}
	fmt.Fprintf(stdout, "%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func formatSimDur(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

type simClient struct {
	base   string
	apiKey string
	tenant string
	http   *http.Client
	poll   time.Duration
}

func (c *simClient) runCase(sc simCase, fileAgent string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	thread, err := c.createThread(ctx)
	if err != nil {
		return "", err
	}
	input := sc.Input
	if input == nil {
		input = map[string]any{}
	}
	runID, err := c.createRun(ctx, thread, sc.agent(fileAgent), input)
	if err != nil {
		return runID, err
	}
	status, err := c.waitTerminal(ctx, runID)
	if err != nil {
		return runID, err
	}
	if sc.Expect != nil && sc.Expect.Status != "" && string(status) != sc.Expect.Status {
		return runID, fmt.Errorf("expected status %s, got %s", sc.Expect.Status, status)
	}
	if sc.Expect != nil && sc.Expect.PolicyEffects != nil {
		events, err := c.auditEvents(ctx, runID)
		if err != nil {
			return runID, err
		}
		if err := matchPolicyEffects(events, *sc.Expect.PolicyEffects); err != nil {
			return runID, err
		}
	}
	return runID, nil
}

func (c *simClient) createThread(ctx context.Context) (string, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/threads", map[string]any{"if_exists": "do_nothing"}, false)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("POST /threads: %s", apiErr(body, status))
	}
	var out struct {
		ThreadID string `json:"thread_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ThreadID == "" {
		return "", fmt.Errorf("POST /threads: missing thread_id")
	}
	return out.ThreadID, nil
}

func (c *simClient) createRun(ctx context.Context, threadID, agent string, input any) (string, error) {
	path := "/threads/" + url.PathEscape(threadID) + "/runs"
	body, status, err := c.do(ctx, http.MethodPost, path, map[string]any{
		"agent_id": agent,
		"input":    input,
	}, true)
	if err != nil {
		return "", err
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(body, &out)
	if status == http.StatusForbidden {
		return out.RunID, fmt.Errorf("%s", apiErr(body, status))
	}
	if status >= 300 {
		return out.RunID, fmt.Errorf("POST /runs: %s", apiErr(body, status))
	}
	if out.RunID == "" {
		return "", fmt.Errorf("POST /runs: missing run_id")
	}
	return out.RunID, nil
}

func (c *simClient) waitTerminal(ctx context.Context, runID string) (models.RunStatus, error) {
	path := "/runs/" + url.PathEscape(runID)
	for {
		body, status, err := c.do(ctx, http.MethodGet, path, nil, false)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("timed out waiting for terminal status")
			}
			return "", err
		}
		if status >= 300 {
			return "", fmt.Errorf("GET /runs: %s", apiErr(body, status))
		}
		var out struct {
			Status models.RunStatus `json:"status"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("GET /runs: invalid JSON")
		}
		if isSimTerminal(out.Status) {
			return out.Status, nil
		}
		timer := time.NewTimer(c.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("timed out waiting for terminal status")
		case <-timer.C:
		}
	}
}

func isSimTerminal(status models.RunStatus) bool {
	return status == models.RunStatusSuccess ||
		status == models.RunStatusError ||
		status == models.RunStatusInterrupted ||
		status == models.RunStatusTimeout
}

func (c *simClient) auditEvents(ctx context.Context, runID string) ([]models.AuditEvent, error) {
	q := url.Values{}
	q.Set("run_id", runID)
	q.Set("limit", "200")
	body, status, err := c.do(ctx, http.MethodGet, "/admin-api/audit-events?"+q.Encode(), nil, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotImplemented {
		return nil, fmt.Errorf("audit search needs SQL")
	}
	if status >= 300 {
		return nil, fmt.Errorf("GET /admin-api/audit-events: %s", apiErr(body, status))
	}
	var events []models.AuditEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("GET /admin-api/audit-events: invalid JSON")
	}
	return events, nil
}

func (c *simClient) do(ctx context.Context, method, path string, payload any, simulation bool) ([]byte, int, error) {
	var rdr io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.tenant != "" {
		req.Header.Set(auth.HeaderTenantID, c.tenant)
	}
	if simulation {
		req.Header.Set(auth.HeaderSimulation, "true")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("timed out waiting for terminal status")
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func apiErr(body []byte, status int) string {
	var parsed struct {
		Message    string `json:"message"`
		Error      string `json:"error"`
		ReasonCode string `json:"reason_code"`
	}
	_ = json.Unmarshal(body, &parsed)
	msg := parsed.Message
	if msg == "" {
		msg = parsed.Error
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	if parsed.ReasonCode != "" {
		return msg + " (" + parsed.ReasonCode + ")"
	}
	return msg
}
