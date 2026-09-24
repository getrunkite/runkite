package api

import (
	"context"
	"errors"

	"github.com/getrunkite/runkite/internal/auth"
)

// errSimulationRequiresAdmin is returned when X-Runkite-Simulation is set
// but the caller is not allowed to mark fixture replay (needs admin, or
// empty permissions when strict_permissions is off, or no client auth).
var errSimulationRequiresAdmin = errors.New("simulation requires admin")

type simulationHeaderKey struct{}

func withSimulationHeader(ctx context.Context, raw string) context.Context {
	if !auth.SimulationRequested(raw) {
		return ctx
	}
	return context.WithValue(ctx, simulationHeaderKey{}, true)
}

func simulationRequested(ctx context.Context) bool {
	v, _ := ctx.Value(simulationHeaderKey{}).(bool)
	return v
}

func (s *Server) allowsSimulation(ctx context.Context) bool {
	strict := false
	if s != nil {
		strict = s.strictPermissions
	}
	return auth.AllowsSimulation(auth.FromContext(ctx), strict)
}
