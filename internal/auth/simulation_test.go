package auth_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestSimulationRequested(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"no", false},
		{"false", false},
	}
	for _, tc := range cases {
		if got := auth.SimulationRequested(tc.in); got != tc.want {
			t.Fatalf("SimulationRequested(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAllowsSimulation(t *testing.T) {
	t.Parallel()
	if !auth.AllowsSimulation(nil, false) {
		t.Fatal("no auth + strict off should allow")
	}
	if auth.AllowsSimulation(nil, true) {
		t.Fatal("no auth + strict on should deny")
	}
	if !auth.AllowsSimulation(&auth.AuthResult{}, false) {
		t.Fatal("empty perms + strict off should allow")
	}
	if auth.AllowsSimulation(&auth.AuthResult{}, true) {
		t.Fatal("empty perms + strict on should deny")
	}
	if auth.AllowsSimulation(&auth.AuthResult{Permissions: []string{"write"}}, false) {
		t.Fatal("write should not allow simulation")
	}
	if auth.AllowsSimulation(&auth.AuthResult{Permissions: []string{"agents:echo:run"}}, false) {
		t.Fatal("agent run grant should not allow simulation")
	}
	if !auth.AllowsSimulation(&auth.AuthResult{Permissions: []string{"admin"}}, true) {
		t.Fatal("admin should allow simulation")
	}
}
