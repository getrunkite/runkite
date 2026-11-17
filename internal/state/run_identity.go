package state

import "github.com/getrunkite/runkite/internal/models"

// RunIdentity identifies the original stored run for internal mutation fences.
// It does not grant the backend SQL-only public observation capabilities.
func RunIdentity(run *models.Run) string {
	if run == nil {
		return ""
	}
	if run.IncarnationID != "" {
		return run.IncarnationID
	}
	return run.StorageIdentity
}
