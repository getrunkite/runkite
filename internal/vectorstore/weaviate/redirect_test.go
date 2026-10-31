package weaviate_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/vectorstore/weaviate"
)

func TestConfiguredEndpointRedirectDoesNotReceiveDelegatedPayload(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, operation := range []string{"init", "upsert"} {
			t.Run(operation+"/"+strconv.Itoa(code), func(t *testing.T) {
				var targetHits, originHits atomic.Int32
				var missingOriginCredentials, originPayload atomic.Bool
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					targetHits.Add(1)
					_, _ = io.Copy(io.Discard, req.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				t.Cleanup(target.Close)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					originHits.Add(1)
					user, password, ok := req.BasicAuth()
					if !ok || user != "synthetic-user" || password != "synthetic-key" {
						missingOriginCredentials.Store(true)
					}
					body, _ := io.ReadAll(req.Body)
					if strings.Contains(string(body), "private vector payload") {
						originPayload.Store(true)
					}
					if operation == "upsert" && req.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Location", target.URL+"/unexpected-recipient")
					w.WriteHeader(code)
				}))
				t.Cleanup(origin.Close)
				endpoint, err := url.Parse(origin.URL)
				if err != nil {
					t.Fatal(err)
				}
				endpoint.User = url.UserPassword("synthetic-user", "synthetic-key")
				store, err := weaviate.New(endpoint.String(), "Vectors", 2)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "init" {
					err = store.Init(context.Background())
				} else {
					err = store.Upsert(context.Background(), &models.VectorItem{
						Namespace: "private", ID: "item", Content: "private vector payload", Embedding: []float32{1, 0},
					})
				}
				if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
					t.Fatalf("redirect must remain an explicit status error: %v", err)
				}
				if targetHits.Load() != 0 {
					t.Fatalf("redirect received request data or credentials: hits=%d", targetHits.Load())
				}
				expected := int32(1)
				if operation == "upsert" {
					expected = 2
				}
				if originHits.Load() != expected || missingOriginCredentials.Load() {
					t.Fatalf("configured-origin request was not exercised: hits=%d missing-auth=%t", originHits.Load(), missingOriginCredentials.Load())
				}
				if operation == "upsert" && !originPayload.Load() {
					t.Fatal("configured origin did not receive the vector payload before redirecting")
				}
			})
		}
	}
}

func TestInitCreatesSchemaOnlyAfterNotFound(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var creates atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					w.WriteHeader(code)
					return
				}
				creates.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(origin.Close)
			store, err := weaviate.New(origin.URL, "Vectors", 2)
			if err != nil {
				t.Fatal(err)
			}
			err = store.Init(context.Background())
			if code == http.StatusNotFound {
				if err != nil || creates.Load() != 1 {
					t.Fatalf("missing schema not created: creates=%d err=%v", creates.Load(), err)
				}
			} else if err == nil || creates.Load() != 0 {
				t.Fatalf("schema probe failure triggered creation: status=%d creates=%d err=%v", code, creates.Load(), err)
			}
		})
	}
}
