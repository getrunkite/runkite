package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestJSONConnectorDoesNotForwardCredentialOrBodyOnRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte(`{}`)) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer origin.Close()
	_, status, err := forwardMCPRequest(context.Background(), origin.URL, "synthetic-token", []byte(`{"secret":"synthetic"}`))
	if err != nil || status != 307 || hits.Load() != 0 {
		t.Fatalf("redirect followed: status=%d hits=%d err=%v", status, hits.Load(), err)
	}
}
