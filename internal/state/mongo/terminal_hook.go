package mongo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var _ state.RunTerminalHookStore = (*Store)(nil)

func (s *Store) TryClaimRunTerminalHook(ctx context.Context, runID, incarnation string) (bool, error) {
	if incarnation == "" {
		return false, &state.ErrConflict{Resource: "run", ID: runID, Reason: "original record identity required for terminal hook claim"}
	}
	claimed := false
	err := s.withTransaction(ctx, func(ctx context.Context) error {
		claimed = false
		run, err := s.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if state.RunIdentity(run) != incarnation {
			return &state.ErrConflict{Resource: "run", ID: runID, Reason: "retained run incarnation changed"}
		}
		if !state.TerminalRunStatus(run.Status) {
			return &state.ErrConflict{Resource: "run", ID: runID, Reason: "terminal hook claim requires a terminal run"}
		}
		key := transport.RunRecordKey(runID, incarnation)
		for _, candidate := range []string{runID, key} {
			err := s.col("terminal_hook_claims").FindOne(ctx, bson.M{"run_id": candidate}).Err()
			if err == nil {
				if candidate == runID {
					return &state.ErrConflict{Resource: "run", ID: runID, Reason: "legacy terminal hook claim is ambiguous; reconcile and use fresh operation IDs"}
				}
				return nil
			}
			if !errors.Is(err, mongo.ErrNoDocuments) {
				return err
			}
		}
		recordHex, ok := strings.CutPrefix(run.StorageIdentity, "mongo-objectid:")
		recordID, err := bson.ObjectIDFromHex(recordHex)
		if !ok || err != nil || recordID.IsZero() {
			return &state.ErrConflict{Resource: "run", ID: runID, Reason: "original Mongo record identity unavailable"}
		}
		// Snapshot reads alone do not fence deletion. Writing this original
		// document makes replacement and concurrent claimers conflict/retry.
		result, err := s.col("runs").UpdateOne(ctx, tenantFilter(ctx, bson.M{"run_id": runID, "_id": recordID}), bson.M{"$inc": bson.M{"_runkite_terminal_hook_revision": int64(1)}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return &state.ErrConflict{Resource: "run", ID: runID, Reason: "retained run incarnation changed"}
		}
		if _, err := s.col("terminal_hook_claims").InsertOne(ctx, bson.M{"run_id": key, "claimed_at": time.Now().UTC()}); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed && err == nil, err
}
