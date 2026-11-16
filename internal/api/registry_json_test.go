package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
)

const registryOpaqueMetadata = `{"id":9007199254740993,"decimal":1.0000000000000001,"nested":[9007199254740993,{"decimal":2.0000000000000001},null,false,"9007199254740993"]}`

func TestRegistryOpaqueJSONPublicAndAdminRoundTrip(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)
	handler := s.Handler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	assertMetadata := func(raw json.RawMessage) {
		t.Helper()
		var got, want interface{}
		if err := jsonvalue.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if err := jsonvalue.Unmarshal([]byte(registryOpaqueMetadata), &want); err != nil {
			t.Fatal(err)
		}
		if !jsonvalue.Equal(got, want) {
			t.Fatalf("opaque metadata changed: %s", raw)
		}
	}
	assertPublic := func(response *httptest.ResponseRecorder) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, exposed := body["metadata_json"]; exposed {
			t.Fatal("Admin transport field leaked into the public API")
		}
		assertMetadata(body["metadata"])
	}
	assertPublic(request(http.MethodPut, "/registry/entries/opaque", `{"source_type":"inline","source_ref":"{}","metadata":`+registryOpaqueMetadata+`}`))
	assertPublic(request(http.MethodGet, "/registry/entries/opaque", ""))

	admin := request(http.MethodGet, "/admin-api/registry/opaque?tenant_id=default", "")
	var view struct {
		MetadataJSON string `json:"metadata_json"`
	}
	if err := json.Unmarshal(admin.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	assertMetadata(json.RawMessage(view.MetadataJSON))
	request(http.MethodPut, "/admin-api/registry/opaque?tenant_id=default", `{"description":"edited","source_type":"inline","source_ref":"{}","metadata":`+view.MetadataJSON+`}`)
	assertPublic(request(http.MethodGet, "/registry/entries/opaque", ""))

	var versions []map[string]json.RawMessage
	if err := json.Unmarshal(request(http.MethodGet, "/registry/entries/opaque/versions", "").Body.Bytes(), &versions); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d immutable versions, want 2", len(versions))
	}
	for _, version := range versions {
		assertMetadata(version["metadata"])
		if _, exposed := version["metadata_json"]; exposed {
			t.Fatal("Admin transport field leaked into public history")
		}
	}
	// This remains replacement PUT. Omitting metadata clears it; preserving the
	// loaded value is the browser's explicit responsibility, not a store mode.
	cleared := request(http.MethodPut, "/registry/entries/opaque", `{"source_type":"inline","source_ref":"{}"}`)
	var entry models.RegistryEntry
	if err := json.Unmarshal(cleared.Body.Bytes(), &entry); err != nil || entry.Metadata != nil {
		t.Fatalf("replacement contract changed: %s %v", cleared.Body.String(), err)
	}
}

func TestReadRegistryJSONRetainsBoundsAndDuplicateKeyBehavior(t *testing.T) {
	for _, body := range []string{
		`{"source_type":"inline","source_ref":"{}"} {}`,
		`{"metadata":`,
		`{"metadata":{"large":"` + strings.Repeat("x", int(maxJSONBodyBytes)) + `"}}`,
	} {
		var entry models.RegistryEntry
		if err := readRegistryEntryJSON(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)), &entry); err == nil {
			t.Fatal("invalid or oversized registry body accepted")
		}
	}
	var entry models.RegistryEntry
	body := `{"source_type":"git","source_type":"inline","metadata":{"keep":true,"id":1},"metadata":{"id":9007199254740993,"nested":[1,2]}}`
	if err := readRegistryEntryJSON(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SourceType != "inline" || entry.Metadata["keep"] != true || entry.Metadata["id"] != json.Number("9007199254740993") {
		t.Fatalf("duplicate key behavior changed: %+v", entry)
	}
}
