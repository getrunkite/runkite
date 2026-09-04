package api

import (
	"context"
	"net/http"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

func validateRegistryEntryBody(entry *models.RegistryEntry) error {
	if entry.SourceType == "" || entry.SourceRef == "" {
		return errRegistrySourceRequired
	}
	switch entry.SourceType {
	case "git", "url", "inline":
		return nil
	default:
		return errRegistrySourceType
	}
}

var (
	errRegistrySourceRequired = &badRequestError{msg: "source_type and source_ref are required"}
	errRegistrySourceType     = &badRequestError{msg: "source_type must be git, url, or inline"}
)

type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

// adminWriteScopedContext scopes Admin mutations to ?tenant_id= (default
// "default"). Writes never use system context — publishing under system
// context would be ambiguous under cross-tenant name collisions.
func adminWriteScopedContext(r *http.Request) context.Context {
	tid := r.URL.Query().Get("tenant_id")
	if tid == "" {
		tid = tenant.DefaultTenant
	}
	return tenant.WithContext(r.Context(), tid)
}

// PUT /admin-api/registry/{name}?tenant_id=
func (s *Server) handleAdminPutRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := adminWriteScopedContext(r)

	var entry models.RegistryEntry
	if err := readJSON(r, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	entry.Name = name
	if err := validateRegistryEntryBody(&entry); err != nil {
		if br, ok := err.(*badRequestError); ok {
			writeError(w, http.StatusBadRequest, br.msg)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.PublishRegistryEntry(ctx, &entry); err != nil {
		handleStoreError(w, err)
		return
	}
	published, err := s.store.GetRegistryEntry(ctx, name)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenant.FromContext(ctx),
		Actor:        actor,
		Action:       "registry.entry.put",
		ResourceType: "registry_entry",
		ResourceID:   name,
		Decision:     policy.EffectAllow,
		ReasonCode:   "registry_entry_put",
		Attrs: map[string]interface{}{
			"source_type": entry.SourceType,
			"version":     published.Version,
		},
	})
	writeJSON(w, http.StatusOK, toAdminRegistryEntryView(published))
}

// DELETE /admin-api/registry/{name}?tenant_id=
func (s *Server) handleAdminDeleteRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := adminWriteScopedContext(r)

	if err := s.store.DeleteRegistryEntry(ctx, name); err != nil {
		handleStoreError(w, err)
		return
	}

	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenant.FromContext(ctx),
		Actor:        actor,
		Action:       "registry.entry.delete",
		ResourceType: "registry_entry",
		ResourceID:   name,
		Decision:     policy.EffectAllow,
		ReasonCode:   "registry_entry_delete",
	})
	w.WriteHeader(http.StatusNoContent)
}
