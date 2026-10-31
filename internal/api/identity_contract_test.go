package api

import (
	"context"
	"errors"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
)

func TestNewRunIDsCannotNormalizeIntoAnotherBinding(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	for _, id := range []string{" run", "run ", "\trun", "run\n", "run\x00id", "run\x7fid", "\u2003run"} {
		t.Run(id, func(t *testing.T) {
			_, _, err := s.createRunCtx(context.Background(), "thread", &models.RunCreate{RunID: id})
			if !errors.Is(err, errInvalidRunID) {
				t.Fatalf("unsafe new run ID reached persistence: %v", err)
			}
		})
	}
	for _, id := range []string{"", "run", "run with space", "run/雪?#%"} {
		if !auth.ValidRunID(id) {
			t.Errorf("supported custom run ID rejected: %q", id)
		}
	}
}
