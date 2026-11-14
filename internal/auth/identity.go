package auth

import (
	"encoding/json"
	"strings"
	"time"
)

// HasVerifiedIdentity distinguishes an authenticated explicit subject from a
// compatibility display name. Permission checks remain separate.
func HasVerifiedIdentity(result *AuthResult) bool {
	return result != nil && !result.identityMissing && strings.TrimSpace(result.Identity) != ""
}

// MarkIdentityUnverified only attenuates provenance. Delegation may retain an
// old display identity without turning it into a verified requester or voter.
func MarkIdentityUnverified(result *AuthResult) {
	if result != nil {
		result.identityMissing = true
		result.Provenance = nil
		result.delegatedJWT = ""
	}
}

// DelegatedJWTAssertion returns only the original credential captured by strict
// JWT authentication. A copied claim or forwarded header cannot establish it.
func DelegatedJWTAssertion(result *AuthResult) string {
	if !HasVerifiedIdentity(result) || result.Provenance == nil || result.Provenance.Subject != result.Identity || !result.Provenance.ExpiresAt.After(time.Now()) {
		return ""
	}
	return result.delegatedJWT
}

// InheritJWTAssertionFromTrustedParent is only for delegation after the caller
// has matched the assignment identity to its verified frozen parent manifest.
// Untrusted on_behalf_of fields must never be passed to this helper.
func InheritJWTAssertionFromTrustedParent(result *AuthResult, assertion string) {
	if result != nil && HasVerifiedIdentity(result) && result.Provenance != nil && result.Provenance.Subject == result.Identity && result.Provenance.ExpiresAt.After(time.Now()) {
		result.delegatedJWT = assertion
	}
}

// Internal session serialization preserves provider provenance across Redis
// and process boundaries. Legacy blobs do not establish verified identity.
func (result AuthResult) MarshalJSON() ([]byte, error) {
	type wire AuthResult
	return json.Marshal(struct {
		wire
		IdentityVerified bool   `json:"identity_verified"`
		DelegatedJWT     string `json:"_runkite_delegated_jwt,omitempty"`
	}{wire: wire(result), IdentityVerified: HasVerifiedIdentity(&result), DelegatedJWT: DelegatedJWTAssertion(&result)})
}

func (result *AuthResult) UnmarshalJSON(raw []byte) error {
	type wire AuthResult
	var value struct {
		wire
		IdentityVerified *bool  `json:"identity_verified"`
		DelegatedJWT     string `json:"_runkite_delegated_jwt"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*result = AuthResult(value.wire)
	result.identityMissing = value.IdentityVerified == nil || !*value.IdentityVerified
	InheritJWTAssertionFromTrustedParent(result, value.DelegatedJWT)
	return nil
}
