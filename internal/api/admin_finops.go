package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// finopsOverlayStore is the SQL Admin surface for durable FinOps overlays.
type finopsOverlayStore interface {
	GetFinOpsOverlay(ctx context.Context) (*models.FinOpsOverlay, error)
	UpsertFinOpsOverlay(ctx context.Context, o *models.FinOpsOverlay) error
	DeleteFinOpsOverlay(ctx context.Context) error
}

func (s *Server) finopsOverlays() (finopsOverlayStore, bool) {
	st, ok := s.store.(finopsOverlayStore)
	return st, ok
}

// FinOps returns the effective (file ∪ overlay) config. Nil when FinOps
// is unset. Safe for concurrent readers; overlays swap via atomic store.
func (s *Server) FinOps() *finops.Config {
	if s == nil {
		return nil
	}
	return s.finopsEffective.Load()
}

// SetFinOps attaches the file-baseline FinOps config and seeds the
// effective pointer. Overlay reload (Admin write / sibling poll) re-merges
// on top of this baseline without a process restart.
func (s *Server) SetFinOps(cfg *finops.Config) {
	s.finopsBaseline = finops.Clone(cfg)
	s.finopsEffective.Store(finops.Clone(cfg))
}

// ReloadFinOpsOverlays loads the SQL overlay and swaps the effective
// config (file baseline ∪ overlay). Called after Admin PUT/DELETE on the
// writing replica, and by the background poll for siblings.
func (s *Server) ReloadFinOpsOverlays(ctx context.Context) {
	store, ok := s.finopsOverlays()
	if !ok {
		return
	}
	row, err := store.GetFinOpsOverlay(ctx)
	if err != nil {
		slog.Warn("finops: reload overlay failed", "error", err)
		return
	}
	var overlay *finops.Config
	if row != nil {
		overlay, err = finops.DecodeOverlayPayload(row.Payload)
		if err != nil {
			slog.Warn("finops: overlay payload decode failed", "error", err)
			return
		}
	}
	s.finopsEffective.Store(finops.Merge(s.finopsBaseline, overlay))
}

// adminFinOpsView is the GET /admin-api/finops response.
type adminFinOpsView struct {
	File       *finops.Config        `json:"file"`
	Overlay    *finops.Config        `json:"overlay"`
	Effective  *finops.Config        `json:"effective"`
	Meta       *models.FinOpsOverlay `json:"meta,omitempty"`
	HasOverlay bool                  `json:"has_overlay"`
}

// GET /admin-api/finops
func (s *Server) handleAdminGetFinOps(w http.ResponseWriter, r *http.Request) {
	store, ok := s.finopsOverlays()
	if !ok {
		writeError(w, http.StatusNotImplemented, "finops overlay CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	row, err := store.GetFinOpsOverlay(ctx)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	view := adminFinOpsView{
		File:      finops.Clone(s.finopsBaseline),
		Effective: finops.Clone(s.FinOps()),
	}
	if view.File == nil {
		view.File = &finops.Config{Pricebook: finops.Pricebook{}, Budgets: finops.Budgets{Tenants: map[string]finops.BudgetCap{}, Agents: map[string]finops.BudgetCap{}}}
	}
	if view.Effective == nil {
		view.Effective = finops.Clone(view.File)
	}
	if row != nil {
		overlay, err := finops.DecodeOverlayPayload(row.Payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "stored finops overlay is corrupt: "+err.Error())
			return
		}
		view.Overlay = overlay
		view.Meta = row
		view.HasOverlay = true
	} else {
		view.Overlay = &finops.Config{Pricebook: finops.Pricebook{}, Budgets: finops.Budgets{Tenants: map[string]finops.BudgetCap{}, Agents: map[string]finops.BudgetCap{}}}
	}
	writeJSON(w, http.StatusOK, view)
}

// PUT /admin-api/finops — replace the live overlay document (validated).
func (s *Server) handleAdminPutFinOps(w http.ResponseWriter, r *http.Request) {
	store, ok := s.finopsOverlays()
	if !ok {
		writeError(w, http.StatusNotImplemented, "finops overlay CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	bodyPtr, err := finops.DecodeOverlayPayload(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body := *bodyPtr
	if err := finops.ValidateOverlay(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := finops.EncodeOverlayPayload(&body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode overlay: "+err.Error())
		return
	}
	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	ctx := tenant.SystemContext(r.Context())
	row := &models.FinOpsOverlay{
		ID:        models.FinOpsOverlayID,
		Payload:   payload,
		UpdatedAt: time.Now().UTC(),
		UpdatedBy: actor,
	}
	if err := store.UpsertFinOpsOverlay(ctx, row); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadFinOpsOverlays(ctx)
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenant.DefaultTenant,
		Actor:        actor,
		Action:       "finops.overlay.put",
		ResourceType: "finops",
		ResourceID:   models.FinOpsOverlayID,
		Decision:     policy.EffectAllow,
		ReasonCode:   "finops_overlay_put",
		Attrs: map[string]interface{}{
			"pricebook_models": len(body.Pricebook),
			"budget_tenants":   len(body.Budgets.Tenants),
			"budget_agents":    len(body.Budgets.Agents),
		},
	})
	s.handleAdminGetFinOps(w, r)
}

// DELETE /admin-api/finops — clear overlay; effective config = file only.
func (s *Server) handleAdminDeleteFinOps(w http.ResponseWriter, r *http.Request) {
	store, ok := s.finopsOverlays()
	if !ok {
		writeError(w, http.StatusNotImplemented, "finops overlay CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	if err := store.DeleteFinOpsOverlay(ctx); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadFinOpsOverlays(ctx)
	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenant.DefaultTenant,
		Actor:        actor,
		Action:       "finops.overlay.delete",
		ResourceType: "finops",
		ResourceID:   models.FinOpsOverlayID,
		Decision:     policy.EffectAllow,
		ReasonCode:   "finops_overlay_delete",
	})
	w.WriteHeader(http.StatusNoContent)
}
