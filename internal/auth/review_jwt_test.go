package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/golang-jwt/jwt/v5"
)

func TestJWTReviewRolesRequireExplicitSubjectAndTenant(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := testJWKS(t, &key.PublicKey)
	provider, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, subject, tenant, permission, method, path string
		status                                          int
	}{
		{"reader", "alice", "tenant-a", auth.GovernanceReadPermission, "GET", "/review-api/pending-actions", 200},
		{"approver", "alice", "tenant-a", auth.GovernanceApprovePermission, "POST", "/review-api/pending-actions/id/approve", 200},
		{"reader-cannot-approve", "alice", "tenant-a", auth.GovernanceReadPermission, "POST", "/review-api/pending-actions/id/approve", 403},
		{"missing-subject", "", "tenant-a", auth.GovernanceApprovePermission, "GET", "/review-api/pending-actions", 403},
		{"literal-unknown-subject", "unknown", "tenant-a", auth.GovernanceReadPermission, "GET", "/review-api/pending-actions", 200},
		{"missing-tenant", "alice", "", auth.GovernanceReadPermission, "GET", "/review-api/pending-actions", 403},
		{"foreign-role", "alice", "tenant-a", "read:messages", "GET", "/review-api/pending-actions", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"permissions": []string{tc.permission}, "exp": time.Now().Add(time.Hour).Unix()}
			if tc.subject != "" {
				claims["sub"] = tc.subject
			}
			if tc.tenant != "" {
				claims["tenant_id"] = tc.tenant
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test-key-1"
			signed, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			handler := auth.Middleware(provider, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}
