package auth

import "net/http"

// NoneProvider is a no-op auth provider that allows all requests as anonymous.
type NoneProvider struct{}

func (n *NoneProvider) Authenticate(r *http.Request) (*AuthResult, error) {
	return &AuthResult{Identity: "anonymous"}, nil
}
