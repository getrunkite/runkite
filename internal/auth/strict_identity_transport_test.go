package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestStrictJWTRejectsUnsafeJWKSTransportBeforeFetch(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(server.Close)
	for _, endpoint := range []string{server.URL, "http://issuer.invalid/jwks", "https://user:password@issuer.invalid/jwks", "https://issuer.invalid/jwks?token=private", "https://issuer.invalid/jwks#fragment"} {
		_, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: endpoint, Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: true})
		if err == nil || hits.Load() != 0 {
			t.Fatal("strict identity fetched or accepted an unsafe JWKS endpoint")
		}
	}
}

func TestStrictJWTLoopbackRequiresExplicitException(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := testJWKS(t, &key.PublicKey)
	if _, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: server.URL, Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: true, AllowInsecureLoopback: true}); err != nil {
		t.Fatalf("explicit local fixture rejected: %v", err)
	}
	// The fixture exception cannot authorize a remote plaintext issuer.
	if _, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: "http://issuer.invalid/jwks", Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: true, AllowInsecureLoopback: true}); err == nil {
		t.Fatal("loopback exception accepted a remote plaintext issuer")
	}
}

func TestStrictJWTDoesNotFollowJWKSRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var originHits, destinationHits atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				destinationHits.Add(1)
				_, _ = w.Write([]byte(`{"keys":[]}`))
			}))
			t.Cleanup(destination.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originHits.Add(1)
				http.Redirect(w, r, destination.URL, status)
			}))
			t.Cleanup(origin.Close)
			_, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: origin.URL, Issuer: "https://issuer.invalid", Audience: "runkite", StrictIdentity: true, AllowInsecureLoopback: true})
			if err == nil || originHits.Load() == 0 || destinationHits.Load() != 0 {
				t.Fatalf("redirect changed JWKS authority: origin=%d destination=%d error=%v", originHits.Load(), destinationHits.Load(), err)
			}
		})
	}
}
