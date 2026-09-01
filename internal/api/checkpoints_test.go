package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// newCheckpointTestEnv wraps the API handler so path thread_id is injected
// as a RunBinding (production middleware does this via Inflight). Fail-closed
// assertBoundThread requires a binding on every /internal/checkpoints call.
func newCheckpointTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.srv.Close()
	env.srv = httptest.NewServer(withTestCheckpointRunBinding(env.apiServer.Handler()))
	t.Cleanup(env.srv.Close)
	return env
}

func withTestCheckpointRunBinding(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/internal/checkpoints/") &&
			auth.RunBindingFromContext(r.Context()) == nil {
			rest := strings.TrimPrefix(path, "/internal/checkpoints/")
			threadID, _, _ := strings.Cut(rest, "/")
			runID := r.Header.Get(auth.HeaderRunID)
			if runID == "" {
				runID = "test-run"
			}
			gen := int64(1)
			if g := r.Header.Get(auth.HeaderGeneration); g != "" {
				if parsed, err := strconv.ParseInt(g, 10, 64); err == nil {
					gen = parsed
				}
			}
			r = r.WithContext(auth.WithRunBinding(r.Context(), &auth.RunBinding{
				RunID: runID, Generation: gen, TenantID: "default",
				ThreadID: threadID, AgentID: "test",
			}))
		}
		next.ServeHTTP(w, r)
	})
}

func TestOpaqueCheckpointProxyCRUD(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")

	thread := &models.Thread{ThreadID: "thr-opaque-1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"v":1,"hello":"world"}`)
	putReq, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-opaque-1/cp-1", bytes.NewReader(payload))
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putReq.Header.Set("X-Runkite-Checkpoint-Framework", "langgraph")
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status=%d", resp.StatusCode)
	}

	getResp, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-opaque-1/cp-1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getResp.StatusCode, body)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("GET body=%q want %q", body, payload)
	}
	if getResp.Header.Get("X-Runkite-Checkpoint-Framework") != "langgraph" {
		t.Fatalf("framework header=%q", getResp.Header.Get("X-Runkite-Checkpoint-Framework"))
	}

	listResp, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-opaque-1")
	if err != nil {
		t.Fatal(err)
	}
	var listed []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK || len(listed) != 1 {
		t.Fatalf("list status=%d len=%d", listResp.StatusCode, len(listed))
	}
	if listed[0]["checkpoint_id"] != "cp-1" {
		t.Fatalf("list id=%v", listed[0]["checkpoint_id"])
	}

	delReq, _ := http.NewRequest("DELETE", env.srv.URL+"/internal/checkpoints/thr-opaque-1/cp-1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status=%d", delResp.StatusCode)
	}

	miss, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-opaque-1/cp-1")
	if err != nil {
		t.Fatal(err)
	}
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status=%d", miss.StatusCode)
	}
}

func TestOpaqueCheckpointRequiresBinding(t *testing.T) {
	env := newTestEnv(t) // raw handler -- no binding stub
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-nobind", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	putReq, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-nobind/cp-1", bytes.NewReader([]byte("x")))
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unbound PUT status=%d body=%s want 401", resp.StatusCode, body)
	}
	var parsed map[string]string
	_ = json.Unmarshal(body, &parsed)
	if parsed["reason_code"] != auth.ReasonRunBindingRequired {
		t.Fatalf("reason_code=%q", parsed["reason_code"])
	}
}

func TestOpaqueCheckpointTooLarge(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-big", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("x", models.MaxOpaqueCheckpointBytes+1)
	putReq, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-big/cp-big", strings.NewReader(big))
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", resp.StatusCode)
	}
}

func TestOpaqueCheckpointThreadMismatch(t *testing.T) {
	env := newTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-bound", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	inflight := map[string]*transport.RunAssignment{
		"run-1": {RunID: "run-1", Generation: 1, TenantID: "default", ThreadID: "thr-bound", GraphID: "g"},
	}
	lookup := authInflight(inflight)
	h := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, env.apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	putReq, _ := http.NewRequest("PUT", srv.URL+"/internal/checkpoints/other-thread/cp-1", bytes.NewReader([]byte("x")))
	putReq.Header.Set(auth.HeaderRunID, "run-1")
	putReq.Header.Set(auth.HeaderGeneration, "1")
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong thread status=%d body=%s", resp.StatusCode, body)
	}
	var parsed map[string]string
	_ = json.Unmarshal(body, &parsed)
	if parsed["reason_code"] != auth.ReasonRunThreadMismatch {
		t.Fatalf("reason_code=%q body=%s", parsed["reason_code"], body)
	}

	okReq, _ := http.NewRequest("PUT", srv.URL+"/internal/checkpoints/thr-bound/cp-1", bytes.NewReader([]byte("ok")))
	okReq.Header.Set(auth.HeaderRunID, "run-1")
	okReq.Header.Set(auth.HeaderGeneration, "1")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusNoContent {
		t.Fatalf("matching thread status=%d", okResp.StatusCode)
	}
}

func TestOpaqueCheckpointETagCAS(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-etag", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	put1, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-etag/cp-1", bytes.NewReader([]byte("v1")))
	resp1, err := http.DefaultClient.Do(put1)
	if err != nil {
		t.Fatal(err)
	}
	etag1 := resp1.Header.Get("ETag")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNoContent || etag1 == "" {
		t.Fatalf("PUT1 status=%d etag=%q", resp1.StatusCode, etag1)
	}

	getResp, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-etag/cp-1")
	if err != nil {
		t.Fatal(err)
	}
	getETag := getResp.Header.Get("ETag")
	getResp.Body.Close()
	if getETag != etag1 {
		t.Fatalf("GET ETag=%q want %q", getETag, etag1)
	}

	put2, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-etag/cp-1", bytes.NewReader([]byte("v2")))
	put2.Header.Set("If-Match", etag1)
	resp2, err := http.DefaultClient.Do(put2)
	if err != nil {
		t.Fatal(err)
	}
	etag2 := resp2.Header.Get("ETag")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent || etag2 == "" || etag2 == etag1 {
		t.Fatalf("PUT2 status=%d etag=%q (etag1=%q)", resp2.StatusCode, etag2, etag1)
	}

	stale, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-etag/cp-1", bytes.NewReader([]byte("stale")))
	stale.Header.Set("If-Match", etag1)
	staleResp, err := http.DefaultClient.Do(stale)
	if err != nil {
		t.Fatal(err)
	}
	staleBody, _ := io.ReadAll(staleResp.Body)
	staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status=%d body=%s want 412", staleResp.StatusCode, staleBody)
	}
}

func TestOpaqueCheckpointLatest(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-latest", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"cp-a", "cp-b"} {
		req, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-latest/"+id, bytes.NewReader([]byte(id)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s status=%d", id, resp.StatusCode)
		}
	}
	// Subgraph ns blob should not win root latest.
	nsKey := "node:sub\x1fcp-ns"
	nsReq, err := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-latest/"+url.PathEscape(nsKey), bytes.NewReader([]byte("ns")))
	if err != nil {
		t.Fatal(err)
	}
	nsResp, err := http.DefaultClient.Do(nsReq)
	if err != nil {
		t.Fatal(err)
	}
	nsResp.Body.Close()
	if nsResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT ns status=%d", nsResp.StatusCode)
	}

	latest, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-latest/latest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(latest.Body)
	latest.Body.Close()
	if latest.StatusCode != http.StatusOK {
		t.Fatalf("latest status=%d body=%s", latest.StatusCode, body)
	}
	if got := latest.Header.Get("X-Runkite-Checkpoint-Id"); got != "cp-b" {
		t.Fatalf("latest id header=%q want cp-b", got)
	}
	if string(body) != "cp-b" {
		t.Fatalf("latest body=%q", body)
	}
	if latest.Header.Get("ETag") == "" {
		t.Fatal("latest missing ETag")
	}

	nsLatest, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-latest/latest?ns=" + url.QueryEscape("node:sub"))
	if err != nil {
		t.Fatal(err)
	}
	nsBody, _ := io.ReadAll(nsLatest.Body)
	nsLatest.Body.Close()
	if nsLatest.StatusCode != http.StatusOK || string(nsBody) != "ns" {
		t.Fatalf("ns latest status=%d body=%s", nsLatest.StatusCode, nsBody)
	}
	if got := nsLatest.Header.Get("X-Runkite-Checkpoint-Id"); got != url.PathEscape(nsKey) {
		t.Fatalf("ns latest id=%q want %q", got, url.PathEscape(nsKey))
	}
}

func TestOpaqueCheckpointRejectsReservedLatestID(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-res", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-res/latest", bytes.NewReader([]byte("x")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT id=latest status=%d body=%s want 400", resp.StatusCode, body)
	}
}

func TestOpaqueCheckpointRejectsWeakIfMatch(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-weak", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	put1, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-weak/cp-1", bytes.NewReader([]byte("v1")))
	resp1, err := http.DefaultClient.Do(put1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("seed PUT status=%d", resp1.StatusCode)
	}

	weak, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-weak/cp-1", bytes.NewReader([]byte("v2")))
	weak.Header.Set("If-Match", `W/"1"`)
	weakResp, err := http.DefaultClient.Do(weak)
	if err != nil {
		t.Fatal(err)
	}
	weakBody, _ := io.ReadAll(weakResp.Body)
	weakResp.Body.Close()
	if weakResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("weak If-Match status=%d body=%s want 400 (must not fall through to unconditional PUT)", weakResp.StatusCode, weakBody)
	}
}

func TestOpaqueCheckpointIfNoneMatchCreateOnly(t *testing.T) {
	env := newCheckpointTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "default")
	thread := &models.Thread{ThreadID: "thr-inom", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}}
	if err := env.store.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	create, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-inom/cp-new", bytes.NewReader([]byte("shell")))
	create.Header.Set("If-None-Match", "*")
	createResp, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusNoContent {
		t.Fatalf("create-only PUT status=%d want 204", createResp.StatusCode)
	}

	again, _ := http.NewRequest("PUT", env.srv.URL+"/internal/checkpoints/thr-inom/cp-new", bytes.NewReader([]byte("clobber")))
	again.Header.Set("If-None-Match", "*")
	againResp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	againBody, _ := io.ReadAll(againResp.Body)
	againResp.Body.Close()
	if againResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second If-None-Match:* status=%d body=%s want 412", againResp.StatusCode, againBody)
	}

	get, err := http.Get(env.srv.URL + "/internal/checkpoints/thr-inom/cp-new")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusOK || string(got) != "shell" {
		t.Fatalf("blob after failed create-only overwrite: status=%d body=%s want shell", get.StatusCode, got)
	}
}

type authInflight map[string]*transport.RunAssignment

func (m authInflight) LookupInflight(_ context.Context, runID string) (*transport.RunAssignment, error) {
	return m[runID], nil
}
