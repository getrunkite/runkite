package sqlcompletion

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

type checkpointTimeRow func(...interface{}) error

func (row checkpointTimeRow) Scan(dest ...interface{}) error { return row(dest...) }

type checkpointTimeTx struct {
	parent       time.Time
	threadLocked bool
	checkpoint   time.Time
	runUpdatedAt time.Time
}

func (tx *checkpointTimeTx) Row(_ context.Context, query string, _ ...interface{}) Row {
	return checkpointTimeRow(func(dest ...interface{}) error {
		switch {
		case strings.Contains(query, "SELECT tenant_id, thread_id, status, metadata FROM runs"):
			*dest[0].(*string) = "default"
			*dest[1].(*string) = "thread"
			*dest[2].(*models.RunStatus) = models.RunStatusRunning
			*dest[3].(*[]byte) = []byte(`{"_runkite_attempt_generation":1}`)
		case strings.Contains(query, "SELECT values_json FROM threads"):
			tx.threadLocked = true
			*dest[0].(*[]byte) = []byte(`{"prior":true}`)
		case strings.Contains(query, "FROM thread_checkpoints"):
			if !tx.threadLocked || len(dest) != 2 {
				return fmt.Errorf("parent timestamp must be read under the merge lock")
			}
			*dest[0].(*sql.NullString) = sql.NullString{String: "previous", Valid: true}
			return dest[1].(*storedTime).Scan(tx.parent)
		case strings.Contains(query, "SELECT status FROM threads"):
			*dest[0].(*string) = "busy"
		case strings.Contains(query, "SELECT COUNT(*) FROM runs"):
			*dest[0].(*int) = 0
		default:
			return fmt.Errorf("unexpected row query: %s", query)
		}
		return nil
	})
}

func (tx *checkpointTimeTx) Exec(_ context.Context, query string, args ...interface{}) error {
	var captured storedTime
	if strings.Contains(query, "INSERT INTO thread_checkpoints") {
		if !tx.threadLocked || args[4] != "previous" {
			return fmt.Errorf("checkpoint lost locked parent")
		}
		if err := captured.Scan(args[10]); err != nil {
			return err
		}
		tx.checkpoint = captured.Time
	}
	if strings.Contains(query, "UPDATE runs SET metadata") {
		if err := captured.Scan(args[4]); err != nil {
			return err
		}
		tx.runUpdatedAt = captured.Time
	}
	return nil
}
func (*checkpointTimeTx) Commit(context.Context) error   { return nil }
func (*checkpointTimeTx) Rollback(context.Context) error { return nil }

func TestCompletionCheckpointTimeFollowsLockedParentAcrossSQLDialects(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			parent := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
			tx := &checkpointTimeTx{parent: parent}
			result, err := (Driver{Dialect: dialect}).CompleteRunInTx(context.Background(), tx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": 42}})
			if err != nil || !result.Applied {
				t.Fatalf("completion=%+v error=%v", result, err)
			}
			if !tx.checkpoint.Equal(parent.Add(time.Microsecond)) {
				t.Fatalf("checkpoint timestamp=%s parent=%s", tx.checkpoint, parent)
			}
			if tx.runUpdatedAt.IsZero() || !tx.runUpdatedAt.Before(parent) {
				t.Fatalf("logical checkpoint floor replaced wall-clock run update: %s", tx.runUpdatedAt)
			}
		})
	}
}

func TestCheckpointTimeRemainsStrictAcrossMicrosecondRounding(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 123456789, time.UTC)
	for _, parent := range []time.Time{now.Truncate(time.Microsecond), now, now.Add(time.Hour)} {
		next := nextCheckpointTime(now, parent)
		if !next.After(parent) || next.Nanosecond()%1000 != 0 {
			t.Fatalf("next=%s does not strictly follow parent=%s at SQL precision", next, parent)
		}
	}
	if got := nextCheckpointTime(now, time.Time{}); !got.Equal(now.Truncate(time.Microsecond)) {
		t.Fatalf("unexpected first timestamp: %s", got)
	}
}
