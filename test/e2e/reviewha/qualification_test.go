package reviewha_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/google/uuid"
)

type actionDetail struct {
	Action models.ConnectorAction        `json:"action"`
	Events []models.ConnectorActionEvent `json:"events"`
}

type reviewLogin struct {
	cookie *http.Cookie
	csrf   string
}

type mcpFixture struct {
	server        *httptest.Server
	writes, holds atomic.Int32
	holdAccepted  chan struct{}
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	target := &mcpFixture{holdAccepted: make(chan struct{}, 1)}
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureCredential {
			w.WriteHeader(401)
			return
		}
		var body struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		var result any = map[string]any{"protocolVersion": "2024-11-05"}
		if body.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "inputSchema": map[string]any{"type": "object"}}, map[string]any{"name": "hold", "inputSchema": map[string]any{"type": "object"}}}}
		}
		if body.Method == "tools/call" {
			switch body.Params.Name {
			case "write":
				target.writes.Add(1)
			case "hold":
				target.holds.Add(1)
				select {
				case target.holdAccepted <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			default:
				w.WriteHeader(400)
				return
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fixtureReply}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
	}))
	t.Cleanup(target.server.Close)
	return target
}

func (s *haSuite) login(p *replica, key string) reviewLogin {
	s.t.Helper()
	r := s.request(p, "POST", "/review-api/session", "", map[string]string{"credential": key}, nil, "")
	s.expect(r, 200)
	body := decode[struct {
		CSRF    string `json:"csrf_token"`
		CanRead bool   `json:"can_read"`
	}](s.t, r)
	response := &http.Response{Header: r.header}
	for _, cookie := range response.Cookies() {
		if cookie.Name == auth.CookieReviewSession && body.CanRead && body.CSRF != "" {
			s.secretValues = append(s.secretValues, cookie.Value, body.CSRF)
			return reviewLogin{cookie, body.CSRF}
		}
	}
	s.t.Fatal("Review login did not issue an authenticated cookie and CSRF token")
	return reviewLogin{}
}

func (s *haSuite) internalHeaders(f tenantFixture, token string) http.Header {
	h := http.Header{}
	h.Set(auth.HeaderRunnerKind, s.runnerKind)
	h.Set(auth.HeaderRunnerToken, s.runnerToken)
	h.Set(auth.HeaderRunID, f.run)
	h.Set(auth.HeaderGeneration, strconv.FormatInt(f.generation, 10))
	if token != "" {
		h.Set(connector.HeaderConnectorSession, token)
	}
	return h
}

func (s *haSuite) connectorSession(p *replica, f tenantFixture) string {
	s.t.Helper()
	r := s.do(s.client, p, "POST", "/internal/connectors/fixture/session", "", map[string]any{}, nil, "", s.internalHeaders(f, ""))
	s.expect(r, 200)
	session := decode[connector.SessionResponse](s.t, r)
	if session.SessionToken == "" || session.Credentials != nil {
		s.t.Fatal("connector session did not return a credential-free capability")
	}
	s.secretValues = append(s.secretValues, session.SessionToken)
	return session.SessionToken
}

func toolBody(tool string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": fixtureRPCID, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{"amount": 123, "apiKey": fixtureArgument}}}
}

func (s *haSuite) tool(p *replica, f tenantFixture, token, tool string) httpResult {
	return s.do(s.client, p, "POST", "/internal/connectors/fixture/mcp", "", toolBody(tool), nil, "", s.internalHeaders(f, token))
}

func (s *haSuite) held(r httpResult) string {
	s.t.Helper()
	s.expect(r, 200)
	body := decode[struct {
		Error struct {
			Data struct {
				Effect string `json:"effect"`
				ID     string `json:"action_id"`
			} `json:"data"`
		} `json:"error"`
	}](s.t, r)
	if body.Error.Data.Effect != "pending" || body.Error.Data.ID == "" {
		s.t.Fatal("synthetic tool was not held for approval")
	}
	return body.Error.Data.ID
}

func (s *haSuite) assertPrivate(raw []byte) {
	s.t.Helper()
	text := string(raw)
	for _, sentinel := range []string{fixtureCredential, fixtureArgument, fixtureRPCID, fixtureReply} {
		if strings.Contains(text, sentinel) {
			s.t.Fatal("Review evidence exposed synthetic sensitive content")
		}
	}
	if strings.Contains(text, `"run_available"`) {
		s.t.Fatal("Review detail unexpectedly exposed full-run availability")
	}
}

func TestReviewAndConnectorEvidenceAcrossRealReplicas(t *testing.T) {
	if os.Getenv("RUNKITE_TEST_REVIEW_HA") != "1" {
		t.Skip("opt in with make test-review-ha on an isolated local or CI test host")
	}
	repetitions := 1
	if raw := os.Getenv("RUNKITE_TEST_REVIEW_REPETITIONS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 20 || strconv.Itoa(value) != raw {
			t.Fatal("RUNKITE_TEST_REVIEW_REPETITIONS must be an integer from 1 to 20")
		}
		repetitions = value
	}
	suite := newHASuite(t)
	target := newMCPFixture(t)
	// Run suite cleanup before closing the synthetic target, including on a fatal
	// assertion while a target request is waiting for the killed peer.
	t.Cleanup(func() {
		for _, p := range suite.replicas {
			suite.stop(p, false)
		}
	})
	fixtures := make([][2]tenantFixture, repetitions)
	keys := map[string]any{}
	adminKey := uuid.NewString()
	suite.secretValues = append(suite.secretValues, adminKey)
	keys[adminKey] = map[string]any{"name": "fixture-global-admin", "permissions": []string{"admin"}, "tenant_id": "default"}
	hitl := []any{}
	for iteration := 0; iteration < repetitions; iteration++ {
		for index := 0; index < 2; index++ {
			suffix := uuid.NewString()
			f := tenantFixture{tenant: "review-ha-" + suffix, run: "run-" + suffix, thread: "thread-" + suffix, generation: 1, readKey: uuid.NewString(), approveKey: uuid.NewString()}
			fixtures[iteration][index] = f
			suite.secretValues = append(suite.secretValues, f.readKey, f.approveKey)
			keys[f.readKey] = map[string]any{"name": "fixture-reader", "tenant_id": f.tenant, "permissions": []string{auth.GovernanceReadPermission}}
			keys[f.approveKey] = map[string]any{"name": "fixture-approver", "tenant_id": f.tenant, "permissions": []string{auth.GovernanceApprovePermission}}
			hitl = append(hitl, map[string]any{"id": "review-" + suffix, "tenant_id": f.tenant, "connector": "fixture", "tools": []string{"write"}})
			suite.seed(f)
		}
	}
	cfg := map[string]any{"runner_kind": suite.runnerKind, "graphs": map[string]any{"ha-agent": "synthetic.py:graph"}, "auth": map[string]any{"type": "api_key", "strict_permissions": true, "keys": keys}, "connectors": map[string]any{"fixture": map[string]any{"auth": map[string]any{"type": "bearer", "bearer_token": fixtureCredential}, "mcp": map[string]any{"url": target.server.URL}}}, "policy": map[string]any{"default_effect": "allow", "audit_required": true, "action_evidence": true, "mandatory_hitl": hitl}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	suite.config = filepath.Join(suite.directory, "runkite.json")
	if err := os.WriteFile(suite.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, b := suite.start(0), suite.start(1)
	if a.command.Process.Pid == b.command.Process.Pid {
		t.Fatal("qualification requires two separate processes")
	}
	suite.report["repetitions"] = repetitions
	suite.report["listeners"] = "serve uses its existing :port listeners; client endpoints and disposable infrastructure mappings use loopback; this is not a network-isolated sandbox"
	suite.record("two_actual_processes_ready", map[string]any{"pid_a": a.command.Process.Pid, "pid_b": b.command.Process.Pid, "client_url_a": a.base, "client_url_b": b.base})
	for iteration, pair := range fixtures {
		t.Run(fmt.Sprintf("iteration_%02d", iteration+1), func(t *testing.T) {
			// Both replicas remain real child processes. A may have been restarted by
			// the previous bounded repetition; all requests select a concrete peer.
			a, b = suite.current[0], suite.current[1]
			suite.iteration(t, target, a, b, pair[0], pair[1], adminKey, iteration+1)
		})
		if t.Failed() {
			break
		}
	}
}

func (s *haSuite) iteration(t *testing.T, target *mcpFixture, a, b *replica, own, other tenantFixture, adminKey string, iteration int) {
	// Helper assertions belong to this subtest, preserving bounded cleanup after
	// a failed iteration. No parallel subtest mutates suite state.
	previous := s.t
	s.t = t
	defer func() { s.t = previous }()
	writesBefore, holdsBefore := target.writes.Load(), target.holds.Load()
	reader := s.login(a, own.readKey)
	approver := s.login(a, own.approveKey)
	ownToken := s.connectorSession(a, own)
	otherToken := s.connectorSession(b, other)
	// Connector capabilities use the actual Redis-backed session store in serve.
	// This is distinct from the public /mcp protocol's session routing contract.
	listBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	cross := s.do(s.client, b, "POST", "/internal/connectors/fixture/mcp", "", listBody, nil, "", s.internalHeaders(own, ownToken))
	s.expect(cross, 200)
	pending := s.held(s.tool(a, own, ownToken, "write"))
	foreign := s.held(s.tool(b, other, otherToken, "write"))
	if target.writes.Load() != writesBefore {
		t.Fatal("pending calls executed before review")
	}
	s.record(fmt.Sprintf("%02d_cross_replica_connector_capability", iteration), map[string]any{"minted_on": "A", "accepted_on": "B"})

	read := s.request(b, "GET", "/review-api/pending-actions/"+pending, "", nil, reader.cookie, "")
	s.expect(read, 200)
	s.assertPrivate(read.body)
	metadata := decode[map[string]json.RawMessage](t, read)
	if _, present := metadata["args"]; present {
		t.Fatal("read-only Review response included arguments")
	}
	if _, present := metadata["reason"]; present {
		t.Fatal("Review response included a free-form reason")
	}
	approveView := s.request(b, "GET", "/review-api/pending-actions/"+pending, "", nil, approver.cookie, "")
	s.expect(approveView, 200)
	s.assertPrivate(approveView.body)
	if len(decode[map[string]json.RawMessage](t, approveView)["args"]) == 0 {
		t.Fatal("approver lost redacted operation preview")
	}
	for _, p := range []*replica{a, b} {
		s.expect(s.request(p, "GET", "/review-api/pending-actions/"+foreign, own.approveKey, nil, nil, ""), 404)
		s.expect(s.request(p, "GET", "/review-api/pending-actions?tenant_id="+other.tenant, own.approveKey, nil, nil, ""), 403)
		s.expect(s.request(p, "GET", "/admin-api/pending-actions", own.approveKey, nil, nil, ""), 403)
		s.expect(s.request(p, "GET", "/threads/"+own.thread, own.approveKey, nil, nil, ""), 403)
		s.expect(s.request(p, "POST", "/review-api/pending-actions/"+pending+"/approve", "", nil, reader.cookie, reader.csrf), 403)
	}
	s.expect(s.request(b, "POST", "/review-api/pending-actions/"+pending+"/approve", "", nil, approver.cookie, ""), 403)
	s.expect(s.request(b, "GET", "/admin-api/pending-actions", "", nil, approver.cookie, ""), 401)
	swappedCookie := *approver.cookie
	swappedCookie.Name = auth.CookieAdminSession
	s.expect(s.request(b, "GET", "/admin-api/pending-actions", "", nil, &swappedCookie, ""), 401)
	s.expect(s.request(b, "GET", "/review-api/pending-actions", "invalid-primary-credential", nil, approver.cookie, ""), 401)
	s.record(fmt.Sprintf("%02d_review_scope_role_and_csrf", iteration), map[string]any{"cookie_created_on": "A", "cookie_used_on": "B", "foreign_id_status": 404, "tenant_override_status": 403})

	var wg sync.WaitGroup
	approvals := make(chan httpResult, 2)
	for _, p := range []*replica{a, b} {
		wg.Add(1)
		go func(p *replica) {
			defer wg.Done()
			approvals <- s.request(p, "POST", "/review-api/pending-actions/"+pending+"/approve", "", nil, approver.cookie, approver.csrf)
		}(p)
	}
	wg.Wait()
	close(approvals)
	approvedCount := 0
	for response := range approvals {
		if response.status == 200 && response.err == nil {
			approvedCount++
			s.assertPrivate(response.body)
		} else {
			s.expect(response, 409)
		}
	}
	if approvedCount != 1 {
		t.Fatalf("approval winners=%d", approvedCount)
	}
	for _, p := range []*replica{a, b} {
		r := s.request(p, "GET", "/review-api/pending-actions/"+pending, own.approveKey, nil, nil, "")
		s.expect(r, 200)
		row := decode[models.PendingAction](t, r)
		if row.Status != models.PendingStatusApproved || row.DecidedBy != "fixture-approver" {
			t.Fatal("approval state or authenticated actor differs across replicas")
		}
	}
	s.record(fmt.Sprintf("%02d_cross_replica_approval_race", iteration), map[string]any{"successful_decisions": approvedCount})

	results := make(chan httpResult, 2)
	for _, p := range []*replica{a, b} {
		wg.Add(1)
		go func(p *replica) { defer wg.Done(); results <- s.tool(p, own, ownToken, "write") }(p)
	}
	wg.Wait()
	close(results)
	actionIDs := map[string]bool{}
	observed := 0
	for result := range results {
		if result.err != nil || (result.status != 200 && result.status != 503) {
			t.Fatalf("unexpected dispatch race result status=%d", result.status)
		}
		id := result.header.Get("X-Runkite-Connector-Action-Id")
		if uuid.Validate(id) != nil {
			t.Fatal("dispatch race did not identify both action attempts")
		}
		actionIDs[id] = true
	}
	if len(actionIDs) != 2 || target.writes.Load() != writesBefore+1 {
		t.Fatalf("authorized dispatch count=%d action_ids=%d", target.writes.Load()-writesBefore, len(actionIDs))
	}
	for id := range actionIDs {
		left := s.request(a, "GET", "/review-api/connector-actions/"+id, own.readKey, nil, nil, "")
		right := s.request(b, "GET", "/review-api/connector-actions/"+id, own.readKey, nil, nil, "")
		s.expect(left, 200)
		s.expect(right, 200)
		s.assertPrivate(left.body)
		s.assertPrivate(right.body)
		if string(left.body) != string(right.body) {
			t.Fatal("durable action evidence differs across replicas")
		}
		detail := decode[actionDetail](t, left)
		if detail.Action.State == models.ConnectorActionResponseObserved {
			observed++
		}
		s.expect(s.request(b, "GET", "/review-api/connector-actions/"+id, other.readKey, nil, nil, ""), 404)
	}
	if observed != 1 {
		t.Fatalf("observed winning operations=%d", observed)
	}
	consumed := s.request(b, "GET", "/review-api/pending-actions/"+pending, own.approveKey, nil, nil, "")
	s.expect(consumed, 200)
	if decode[models.PendingAction](t, consumed).Status != models.PendingStatusConsumed {
		t.Fatal("one-shot capability was not consumed durably")
	}
	s.record(fmt.Sprintf("%02d_cross_replica_dispatch_and_evidence", iteration), map[string]any{"authorized_effects": target.writes.Load() - writesBefore, "action_attempts": len(actionIDs), "response_observed": observed})

	// A target acceptance is deliberately not treated as proof that the plane
	// observed a response. Kill the process with an unconfirmed dispatch intent.
	holdResult := make(chan httpResult, 1)
	holdClient := &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer holdClient.CloseIdleConnections()
	go func() {
		holdResult <- s.do(holdClient, a, "POST", "/internal/connectors/fixture/mcp", "", toolBody("hold"), nil, "", s.internalHeaders(own, ownToken))
	}()
	select {
	case <-target.holdAccepted:
	case <-time.After(10 * time.Second):
		t.Fatal("synthetic target never accepted the blocked-response call")
	}
	var unknownID string
	s.wait("durable dispatch intent on surviving peer", 5*time.Second, func() bool {
		response := s.request(b, "GET", "/review-api/connector-actions?run_id="+own.run, own.readKey, nil, nil, "")
		s.expect(response, 200)
		for _, action := range decode[[]models.ConnectorAction](t, response) {
			if action.Tool == "hold" && action.State == models.ConnectorActionDispatchIntent {
				unknownID = action.ID
				return true
			}
		}
		return false
	})
	s.stop(a, true)
	select {
	case response := <-holdResult:
		if response.err == nil {
			t.Fatal("killed control plane unexpectedly delivered a complete response")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("caller did not observe process loss")
	}
	// The current dispatch receipt deadline is 45s and real maintenance ticks every
	// 30s. Leave A dead until B reconciles, rather than editing SQL clocks or state.
	s.wait("surviving peer to reconcile unconfirmed intent", 85*time.Second, func() bool {
		select {
		case <-a.done:
		default:
			t.Fatal("victim control plane unexpectedly restarted")
		}
		response := s.request(b, "GET", "/review-api/connector-actions/"+unknownID, own.readKey, nil, nil, "")
		s.expect(response, 200)
		s.assertPrivate(response.body)
		return decode[actionDetail](t, response).Action.State == models.ConnectorActionOutcomeUnknown
	})
	if target.holds.Load() != holdsBefore+1 {
		t.Fatal("unconfirmed operation was replayed")
	}
	restarted := s.start(0)
	oldCookie := s.request(restarted, "GET", "/review-api/pending-actions/"+pending, "", nil, approver.cookie, "")
	s.expect(oldCookie, 200)
	evidence := s.request(restarted, "GET", "/review-api/connector-actions/"+unknownID, "", nil, reader.cookie, "")
	s.expect(evidence, 200)
	s.assertPrivate(evidence.body)
	detail := decode[actionDetail](t, evidence)
	if detail.Action.State != models.ConnectorActionOutcomeUnknown || len(detail.Events) < 4 || detail.Events[len(detail.Events)-1].Observer != "runkite_reconciler" {
		t.Fatal("reconciled evidence did not survive process restart")
	}
	if target.holds.Load() != holdsBefore+1 {
		t.Fatal("restart replayed the operation")
	}
	s.record(fmt.Sprintf("%02d_process_loss_unknown_and_restart", iteration), map[string]any{"action_id": unknownID, "target_acceptances": target.holds.Load() - holdsBefore, "state": detail.Action.State, "original_pid": a.command.Process.Pid, "replacement_pid": restarted.command.Process.Pid})

	s.expect(s.request(b, "DELETE", "/review-api/session", "", nil, approver.cookie, approver.csrf), 204)
	s.expect(s.request(restarted, "GET", "/review-api/pending-actions", "", nil, approver.cookie, ""), 401)
	s.expect(s.request(restarted, "GET", "/review-api/pending-actions", "", nil, reader.cookie, ""), 200)
	// Primary bearer credentials and global Admin keep their independent authority.
	s.expect(s.request(restarted, "GET", "/review-api/pending-actions", own.approveKey, nil, nil, ""), 200)
	s.expect(s.request(restarted, "GET", "/admin-api/pending-actions", adminKey, nil, nil, ""), 200)
	s.record(fmt.Sprintf("%02d_cross_replica_logout", iteration), map[string]any{"revoked_on": "B", "rejected_on": "restarted A"})
}
