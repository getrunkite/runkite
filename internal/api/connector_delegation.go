package api

import (
	"net/http"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
)

// A constrained connector uses the assertion frozen by the authenticating
// admission path. A worker may not replace Alice's subject with Bob's token.
func bindConnectorDelegation(r *http.Request, registry *connector.Registry, name string) *http.Request {
	c, err := registry.Get(name)
	if err != nil || c.Config.Auth.TokenExchange == nil {
		return r
	}
	assertion := ""
	if binding := auth.RunBindingFromContext(r.Context()); binding != nil && binding.User != nil {
		assertion = binding.User.DelegatedJWTAssertion
	}
	return r.WithContext(connector.WithDelegatedSubject(r.Context(), assertion))
}
