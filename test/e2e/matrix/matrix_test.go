// Package matrix runs independent example agents for every framework
// across every DB/transport combination, so control-plane visibility is
// proven end-to-end, not assumed from adapter unit tests. See spec.go's
// doc comment for why this is a bounded matrix (4 backend combinations
// x 6 framework runners x the scenarios each framework's example agents
// actually support) rather than a full cross-product -- backend
// correctness itself is already exhaustively proven by
// internal/state/conformance and internal/transport/conformance; this
// package proves the integration seam those can't see.
//
// Gated behind RUNKITE_RUN_MATRIX=1 (see TestMain) -- 32 real
// subprocess-pair start/stop cycles is deliberately not something
// `make test-e2e` or a default `go test ./...` should pay for on every
// run; it's meant for `make test-matrix` / the nightly Matrix workflow.
// Individual backend cells still skip gracefully (not fail) if their
// required env var isn't set, same convention as every other
// conformance suite here.
package matrix

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("RUNKITE_RUN_MATRIX") == "" {
		fmt.Println("test/e2e/matrix: RUNKITE_RUN_MATRIX not set -- skipping (see `make test-matrix`)")
		os.Exit(0)
	}
	// Start clean too: a previous crashed matrix / manual redis-cli
	// experiment leaves rk:* keys that flake redis-transport HITL on
	// the very first cell (see cleanupRedis).
	cleanupSharedBackends()
	code := m.Run()
	// Runs regardless of pass/fail -- a failing matrix run still
	// registered agents in the shared backends and still needs to leave
	// them clean for ../e2e_test.go and ../adapters/main_test.go (see
	// cleanupSharedBackends's doc comment).
	cleanupSharedBackends()
	os.Exit(code)
}

func TestMatrix(t *testing.T) {
	for _, backend := range backends {
		t.Run(backend.Name, func(t *testing.T) {
			for _, runner := range runners {
				t.Run(runner.Name, func(t *testing.T) {
					c := startCell(t, backend, runner)
					for _, scenario := range runner.Scenarios {
						t.Run(string(scenario), func(t *testing.T) {
							got := runScenario(t, c, scenario)
							goldenName := backend.Name + "__" + runner.Name + "__" + string(scenario)
							checkGolden(t, goldenName, got)
						})
					}
				})
			}
		})
	}
}
