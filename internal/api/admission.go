package api

import (
	"context"
	"fmt"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// killSwitchStore is the SQL surface for blast-radius kill / pause flags.
type killSwitchStore interface {
	UpsertKillSwitch(ctx context.Context, k *models.KillSwitch) error
	GetKillSwitch(ctx context.Context, id string) (*models.KillSwitch, error)
	DeleteKillSwitch(ctx context.Context, id string) error
	ListKillSwitches(ctx context.Context) ([]*models.KillSwitch, error)
	SearchKillSwitches(ctx context.Context, req *models.KillSwitchSearchRequest) ([]*models.KillSwitch, error)
	FindActiveKill(ctx context.Context, tenantID, agentID string) (*models.KillSwitch, error)
}

func (s *Server) killSwitches() (killSwitchStore, bool) {
	ks, ok := s.store.(killSwitchStore)
	return ks, ok
}

// admissionGate is a hooks.Gate registered into the same CheckBeforeRun
// pipeline as webhook preflight_hooks. Order: kill → agent authz →
// optional policy Decide(run.create). First deny wins.
type admissionGate struct {
	server *Server
}

// RegisterAdmissionGate installs the run-admission Gate on dispatcher.
// Safe when policy is nil (authz + kill still apply).
func (s *Server) RegisterAdmissionGate(d *hooks.Dispatcher) {
	if s == nil || d == nil {
		return
	}
	d.RegisterGate(&admissionGate{server: s})
}

func (g *admissionGate) Decide(ctx context.Context, event hooks.Event) hooks.Decision {
	if g == nil || g.server == nil {
		return hooks.Decision{Allow: true}
	}
	s := g.server
	tenantID := event.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	agentID := event.AgentID

	if store, ok := s.killSwitches(); ok {
		k, err := store.FindActiveKill(ctx, tenantID, agentID)
		if err != nil {
			return hooks.Decision{Allow: false, Reason: "kill switch lookup failed: " + err.Error()}
		}
		if k != nil {
			reason := k.Reason
			if reason == "" {
				if k.AgentID != "" {
					reason = "agent kill switch active"
				} else {
					reason = "tenant kill switch active"
				}
			}
			if k.PauseOnly {
				reason = "enqueue paused: " + reason
			}
			return hooks.Decision{Allow: false, Reason: reason}
		}
	}

	if !auth.CanRunAgent(auth.FromContext(ctx), agentID) {
		return hooks.Decision{
			Allow:  false,
			Reason: fmt.Sprintf("missing permission %s", auth.AgentRunPermission(agentID)),
		}
	}

	if s.policy.Enabled() {
		principal := ""
		if ar := auth.FromContext(ctx); ar != nil {
			principal = ar.Identity
		}
		in := policy.PolicyInput{
			Stage:     policy.StageRunCreate,
			TenantID:  tenantID,
			AgentID:   agentID,
			RunID:     event.RunID,
			Principal: principal,
		}
		if s.tryBreakGlassBypass(ctx, in) {
			return hooks.Decision{Allow: true}
		}
		dec := s.policy.Decide(ctx, in)
		if dec.Effect != policy.EffectAllow {
			reason := dec.Reason
			if reason == "" {
				reason = dec.ReasonCode
			}
			if reason == "" {
				reason = "run.create denied by policy"
			}
			return hooks.Decision{Allow: false, Reason: reason}
		}
	}

	return hooks.Decision{Allow: true}
}
