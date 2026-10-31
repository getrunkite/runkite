package pinecone_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/vectorstore/pinecone"
)

func TestConfiguredEndpointRedirectDoesNotReceiveAPIKeyOrPayload(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, operation := range []string{"init", "upsert"} {
			t.Run(operation+"/"+strconv.Itoa(code), func(t *testing.T) {
				var targetHits, originHits atomic.Int32
				var missingOriginKey, originPayload atomic.Bool
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					targetHits.Add(1)
					_, _ = io.Copy(io.Discard, req.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				t.Cleanup(target.Close)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					originHits.Add(1)
					if req.Header.Get("Api-Key") != "synthetic-api-key" {
						missingOriginKey.Store(true)
					}
					body, _ := io.ReadAll(req.Body)
					if strings.Contains(string(body), "private vector payload") {
						originPayload.Store(true)
					}
					if operation == "upsert" && req.Method == http.MethodGet {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]string{"host": req.Host})
						return
					}
					w.Header().Set("Location", target.URL+"/unexpected-recipient")
					w.WriteHeader(code)
				}))
				t.Cleanup(origin.Close)
				store, err := pinecone.New(origin.URL, "synthetic-api-key", "vectors", 2, "", "")
				if err != nil {
					t.Fatal(err)
				}
				err = store.Init(context.Background())
				if operation == "upsert" {
					if err != nil {
						t.Fatal(err)
					}
					err = store.Upsert(context.Background(), &models.VectorItem{
						Namespace: "private", ID: "item", Content: "private vector payload", Embedding: []float32{1, 0},
					})
				}
				if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
					t.Fatalf("redirect must remain an explicit status error: %v", err)
				}
				if targetHits.Load() != 0 {
					t.Fatalf("redirect received API key or payload: hits=%d", targetHits.Load())
				}
				expected := int32(1)
				if operation == "upsert" {
					expected = 2
				}
				if originHits.Load() != expected || missingOriginKey.Load() {
					t.Fatalf("configured endpoint was not exercised: hits=%d missing-key=%t", originHits.Load(), missingOriginKey.Load())
				}
				if operation == "upsert" && !originPayload.Load() {
					t.Fatal("configured origin did not receive the vector payload before redirecting")
				}
			})
		}
	}
}
