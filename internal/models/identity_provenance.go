package models

import "time"

// IdentityProvenance records bounded claims from a validated, audience-bound
// JWT. Actor claims are assertions by that issuer, not proof of actor key use.
// It deliberately excludes bearer values, arbitrary claims and token IDs.
type IdentityProvenance struct {
	Issuer    string          `json:"issuer"`
	Subject   string          `json:"subject"`
	Audience  string          `json:"audience"`
	ExpiresAt time.Time       `json:"expires_at"`
	Actors    []IdentityActor `json:"issuer_asserted_actors,omitempty"`
}

type IdentityActor struct {
	Subject string `json:"subject"`
	Issuer  string `json:"issuer,omitempty"`
}

// DelegationContract records operator configuration, not a successful token
// exchange or cryptographic verification of the configured actor identity.
type DelegationContract struct {
	Audiences       []string `json:"audiences,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	Scopes          []string `json:"scopes"`
	ConfiguredActor string   `json:"configured_actor,omitempty"`
	MaxTTLSeconds   int64    `json:"max_ttl_seconds"`
}

// WorkloadProvenance identifies the externally certified holder of a dispatch.
// It contains public certificate facts, never a private key or bearer token.
type WorkloadProvenance struct {
	URI       string    `json:"uri"`
	KeySHA256 string    `json:"key_sha256"`
	NotAfter  time.Time `json:"not_after"`
}

func CloneIdentityProvenance(p *IdentityProvenance) *IdentityProvenance {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Actors = append([]IdentityActor(nil), p.Actors...)
	return &copy
}
