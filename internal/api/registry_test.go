package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sharanharsoor/runkite/internal/models"
)

func doDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func doPut(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRegistry_PublishAndGet(t *testing.T) {
	env := newTestEnv(t)

	resp := doPut(t, env.srv.URL+"/registry/entries/sales-qualifier", map[string]interface{}{
		"display_name": "Sales Qualifier",
		"description":  "Qualifies inbound leads",
		"author":       "alice",
		"tags":         []string{"sales", "lead-gen"},
		"source_type":  "git",
		"source_ref":   "https://github.com/example/sales-qualifier",
	})
	defer resp.Body.Close()
	expectStatus(t, resp, 200)

	var published models.RegistryEntry
	json.Unmarshal(readBody(t, resp), &published)
	if published.Name != "sales-qualifier" || published.Version != 1 {
		t.Fatalf("expected name=sales-qualifier version=1, got %+v", published)
	}

	getResp, err := http.Get(env.srv.URL + "/registry/entries/sales-qualifier")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	expectStatus(t, getResp, 200)
	var got models.RegistryEntry
	json.Unmarshal(readBody(t, getResp), &got)
	if got.Author != "alice" || len(got.Tags) != 2 {
		t.Fatalf("expected roundtrip content, got %+v", got)
	}
}

func TestRegistry_PublishMissingSourceRejected(t *testing.T) {
	env := newTestEnv(t)
	resp := doPut(t, env.srv.URL+"/registry/entries/e1", map[string]interface{}{"display_name": "no source"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing source_type/source_ref, got %d", resp.StatusCode)
	}
}

func TestRegistry_GetNonexistent404(t *testing.T) {
	env := newTestEnv(t)
	resp, _ := http.Get(env.srv.URL + "/registry/entries/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRegistry_RepublishBumpsVersionAndTracksHistory(t *testing.T) {
	env := newTestEnv(t)
	body := map[string]interface{}{"display_name": "v1", "source_type": "url", "source_ref": "http://example.com"}
	doPut(t, env.srv.URL+"/registry/entries/e2", body).Body.Close()

	body["display_name"] = "v2"
	resp := doPut(t, env.srv.URL+"/registry/entries/e2", body)
	defer resp.Body.Close()
	var v2 models.RegistryEntry
	json.Unmarshal(readBody(t, resp), &v2)
	if v2.Version != 2 {
		t.Fatalf("expected version 2 after content change, got %d", v2.Version)
	}

	histResp, err := http.Get(env.srv.URL + "/registry/entries/e2/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer histResp.Body.Close()
	expectStatus(t, histResp, 200)
	var versions []models.RegistryEntryVersion
	json.Unmarshal(readBody(t, histResp), &versions)
	if len(versions) != 2 {
		t.Fatalf("expected 2 version snapshots, got %d", len(versions))
	}
	if versions[0].Version != 2 || versions[0].DisplayName != "v2" {
		t.Errorf("expected newest-first with v2 first, got %+v", versions[0])
	}

	v1Resp, err := http.Get(env.srv.URL + "/registry/entries/e2/versions/1")
	if err != nil {
		t.Fatal(err)
	}
	defer v1Resp.Body.Close()
	expectStatus(t, v1Resp, 200)
	var v1 models.RegistryEntryVersion
	json.Unmarshal(readBody(t, v1Resp), &v1)
	if v1.DisplayName != "v1" {
		t.Errorf("expected v1 snapshot display_name=v1, got %+v", v1)
	}
}

func TestRegistry_SearchByTagsAndAuthor(t *testing.T) {
	env := newTestEnv(t)
	doPut(t, env.srv.URL+"/registry/entries/sales-bot", map[string]interface{}{
		"author": "alice", "tags": []string{"sales"}, "source_type": "git", "source_ref": "x",
	}).Body.Close()
	doPut(t, env.srv.URL+"/registry/entries/support-bot", map[string]interface{}{
		"author": "bob", "tags": []string{"support"}, "source_type": "git", "source_ref": "x",
	}).Body.Close()

	resp, err := postJSON(env.srv.URL+"/registry/search", map[string]interface{}{"author": "bob"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	var results []models.RegistryEntry
	json.Unmarshal(readBody(t, resp), &results)
	if len(results) != 1 || results[0].Name != "support-bot" {
		t.Fatalf("expected only support-bot for author=bob, got %+v", results)
	}
}

func TestRegistry_Delete(t *testing.T) {
	env := newTestEnv(t)
	doPut(t, env.srv.URL+"/registry/entries/e3", map[string]interface{}{"source_type": "url", "source_ref": "x"}).Body.Close()

	delResp := doDelete(t, env.srv.URL+"/registry/entries/e3")
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", delResp.StatusCode)
	}

	getResp, _ := http.Get(env.srv.URL + "/registry/entries/e3")
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getResp.StatusCode)
	}
}
