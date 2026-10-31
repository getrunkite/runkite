package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

type failedLogoutStore struct {
	auth.SessionStore
	fail bool
}

func (s *failedLogoutStore) Delete(id string) error {
	if s.fail {
		return errors.New("synthetic unavailable session backend")
	}
	return s.SessionStore.Delete(id)
}

func TestLogoutRequiresConfirmedRevocation(t *testing.T) {
	memory := auth.NewAdminSessionStore(0)
	session, err := memory.Create(&auth.AuthResult{Identity: "operator", Permissions: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	store := &failedLogoutStore{SessionStore: memory, fail: true}
	h := auth.AdminSessionHandlers{Store: store}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, "/admin-api/session", nil)
		r.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: session.ID})
		w := httptest.NewRecorder()
		h.Delete(w, r)
		return w
	}
	w := request()
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("false logout confirmation: %d %s", w.Code, w.Body.String())
	}
	if memory.Get(session.ID) == nil {
		t.Fatal("failed backend unexpectedly revoked session")
	}
	store.fail = false
	w = request()
	if w.Code != http.StatusNoContent || memory.Get(session.ID) != nil {
		t.Fatal("successful retry did not revoke session")
	}
	if w.Header().Get("Set-Cookie") == "" {
		t.Fatal("confirmed logout must expire cookie")
	}
}
