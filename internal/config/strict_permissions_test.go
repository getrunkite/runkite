package config

import "testing"

func TestEffectiveStrictPermissions(t *testing.T) {
	t.Parallel()
	trueVal, falseVal := true, false

	cases := []struct {
		name string
		auth *AuthEntry
		want bool
	}{
		{"nil auth", nil, false},
		{"no type unset", &AuthEntry{}, false},
		{"api_key unset defaults true", &AuthEntry{Type: "api_key"}, true},
		{"jwt unset defaults true", &AuthEntry{Type: "jwt"}, true},
		{"webhook unset defaults true", &AuthEntry{Type: "webhook"}, true},
		{"api_key explicit false", &AuthEntry{Type: "api_key", StrictPermissions: &falseVal}, false},
		{"none explicit true", &AuthEntry{Type: "none", StrictPermissions: &trueVal}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.auth.EffectiveStrictPermissions(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
