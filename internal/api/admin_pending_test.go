package api_test

import (
	"net/http"
	"testing"
)

func TestAdminPendingActions_UnsupportedStoreReturns501(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/admin-api/pending-actions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}
