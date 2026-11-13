package auth

import (
	"encoding/json"
	"strings"
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
	}
}

// Internal session serialization preserves provider provenance across Redis
// and process boundaries. Legacy blobs do not establish verified identity.
func (result AuthResult) MarshalJSON() ([]byte, error) {
	type wire AuthResult
	return json.Marshal(struct {
		wire
		IdentityVerified bool `json:"identity_verified"`
	}{wire: wire(result), IdentityVerified: HasVerifiedIdentity(&result)})
}

func (result *AuthResult) UnmarshalJSON(raw []byte) error {
	type wire AuthResult
	var value struct {
		wire
		IdentityVerified *bool `json:"identity_verified"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*result = AuthResult(value.wire)
	result.identityMissing = value.IdentityVerified == nil || !*value.IdentityVerified
	return nil
}
