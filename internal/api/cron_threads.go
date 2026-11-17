package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// canonicalCronThreadID keeps named tenants outside the legacy cron: namespace.
// Length framing preserves exact identifier pairs, while hashing bounds the ID
// independently of tenant and schedule lengths for every store backend.
func canonicalCronThreadID(tenantID, scheduleName string) string {
	framed := []byte("runkite/cron-thread/v1\x00")
	for _, value := range []string{tenantID, scheduleName} {
		framed = binary.BigEndian.AppendUint64(framed, uint64(len(value)))
		framed = append(framed, value...)
	}
	digest := sha256.Sum256(framed)
	return "cron-tenant:v1:" + hex.EncodeToString(digest[:])
}

// resolveCronThread preserves an owned legacy conversation until a canonical
// thread exists. Canonical then wins, so a later legacy row cannot redirect it.
// Reads never adopt a foreign tenant's thread, including under system callers.
// A nil thread with nil error means the selected ID has no retained owned row.
func (s *Server) resolveCronThread(ctx context.Context, scheduleName string) (string, *models.Thread, error) {
	tenantID := tenant.FromContext(ctx)
	ctx = tenant.ScopedContext(ctx, tenantID)
	legacyID := "cron:" + scheduleName
	if tenantID != tenant.DefaultTenant {
		canonicalID := canonicalCronThreadID(tenantID, scheduleName)
		thread, err := s.store.GetThread(ctx, canonicalID)
		if err == nil {
			return canonicalID, thread, nil
		}
		var missing *state.ErrNotFound
		if !errors.As(err, &missing) {
			return canonicalID, nil, err
		}
		thread, err = s.store.GetThread(ctx, legacyID)
		if err == nil {
			return legacyID, thread, nil
		}
		if !errors.As(err, &missing) {
			return canonicalID, nil, err
		}
		return canonicalID, nil, nil
	}
	thread, err := s.store.GetThread(ctx, legacyID)
	var missing *state.ErrNotFound
	if errors.As(err, &missing) {
		return legacyID, nil, nil
	}
	return legacyID, thread, err
}
