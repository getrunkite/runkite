package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdminPendingActions_NonSQLStoreReturns501(t *testing.T) {
	srv := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/pending-actions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}
