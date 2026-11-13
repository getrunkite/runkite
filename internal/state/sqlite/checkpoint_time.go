package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

func checkpointTimestamp(value *string) (string, error) {
	instant := time.Now().UTC()
	if value != nil && *value != "" {
		var err error
		instant, err = time.Parse(time.RFC3339Nano, *value)
		if err != nil {
			return "", fmt.Errorf("checkpoint created_at must be a valid RFC3339 timestamp")
		}
	}
	return instant.UTC().Format(sqlcompletion.SQLiteTimestampLayout), nil
}

// Normalize retained timestamps without changing their instants or payloads.
// Keyset pages bound memory; one write transaction keeps every ordering reader
// on the old or new representation. Down need not restore a lossy text format.
func (s *SQLiteStore) normalizeCheckpointTimes(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	var after *string
	for {
		query := `SELECT checkpoint_id,created_at FROM thread_checkpoints`
		var args []any
		if after != nil {
			query += ` WHERE checkpoint_id > ?`
			args = append(args, *after)
		}
		query += ` ORDER BY checkpoint_id LIMIT 256`
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		type update struct{ id, timestamp string }
		var updates []update
		count := 0
		for rows.Next() {
			var id, old string
			if err := rows.Scan(&id, &old); err != nil {
				rows.Close()
				return err
			}
			// Unlike a new write, an empty legacy timestamp must not acquire a
			// fabricated current time. Stop the upgrade for explicit repair.
			instant, err := time.Parse(time.RFC3339Nano, old)
			if err != nil {
				rows.Close()
				return fmt.Errorf("checkpoint %q has invalid created_at; repair it before upgrading", id)
			}
			normalized := instant.UTC().Format(sqlcompletion.SQLiteTimestampLayout)
			if normalized != old {
				updates = append(updates, update{id, normalized})
			}
			after = &id
			count++
		}
		readErr := rows.Err()
		rows.Close()
		if readErr != nil {
			return readErr
		}
		for _, value := range updates {
			if _, err := conn.ExecContext(ctx, `UPDATE thread_checkpoints SET created_at=? WHERE checkpoint_id=?`, value.timestamp, value.id); err != nil {
				return err
			}
		}
		if count < 256 {
			break
		}
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}
