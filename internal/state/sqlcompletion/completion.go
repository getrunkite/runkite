// Package sqlcompletion shares the transactional completion invariant between
// SQL backends. Drivers supply only transaction and placeholder adapters.
package sqlcompletion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

type Row interface{ Scan(...interface{}) error }
type Tx interface {
	Exec(context.Context, string, ...interface{}) error
	Row(context.Context, string, ...interface{}) Row
	Commit(context.Context) error
	Rollback(context.Context) error
}
type Driver struct {
	Begin          func(context.Context) (Tx, error)
	Dialect        string
	ValidateValues func(context.Context, Tx, []byte) error
}

func (d Driver) query(q string) string {
	if d.Dialect != "postgres" {
		return q
	}
	n := 0
	return replaceQuestions(q, func() string { n++; return fmt.Sprintf("$%d", n) })
}
func replaceQuestions(q string, next func() string) string {
	var out strings.Builder
	for _, r := range q {
		if r == '?' {
			out.WriteString(next())
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func (d Driver) now(t time.Time) interface{} {
	if d.Dialect == "sqlite" {
		return t.Format(time.RFC3339Nano)
	}
	return t
}
func (d Driver) lock() string {
	if d.Dialect == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}
func (d Driver) load(ctx context.Context, tx Tx, runID string) (string, string, models.RunStatus, map[string]interface{}, error) {
	q := `SELECT tenant_id, thread_id, status, metadata FROM runs WHERE run_id = ?`
	args := []interface{}{runID}
	if !tenant.IsSystem(ctx) {
		q += ` AND tenant_id = ?`
		args = append(args, tenant.FromContext(ctx))
	}
	var tid, threadID string
	var st models.RunStatus
	var raw []byte
	err := tx.Row(ctx, d.query(q+d.lock()), args...).Scan(&tid, &threadID, &st, &raw)
	if err != nil {
		return "", "", "", nil, err
	}
	meta := map[string]interface{}{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return "", "", "", nil, err
		}
	}
	if meta == nil {
		meta = map[string]interface{}{}
	}
	return tid, threadID, st, meta, nil
}
func (d Driver) BeginRunAttempt(ctx context.Context, runID string, generation int64) (bool, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tid, _, st, meta, err := d.load(ctx, tx, runID)
	if err != nil {
		return false, err
	}
	if state.TerminalRunStatus(st) || state.MetadataGeneration(meta, state.AttemptGenerationKey) > generation {
		return false, nil
	}
	meta[state.AttemptGenerationKey] = generation
	raw, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	if err := tx.Exec(ctx, d.query(`UPDATE runs SET metadata = ?, status = ?, updated_at = ? WHERE run_id = ? AND tenant_id = ?`), raw, string(models.RunStatusRunning), d.now(time.Now().UTC()), runID, tid); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
func (d Driver) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return state.CompletionResult{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tid, threadID, st, meta, err := d.load(ctx, tx, c.RunID)
	if err != nil {
		return state.CompletionResult{}, err
	}
	c = state.ResolveCompletion(meta, c)
	result := state.CheckCompletion(meta, st, c)
	if !result.Applied {
		return result, nil
	}
	now := time.Now().UTC()
	meta[state.CompletionGenerationKey] = c.Generation
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return state.CompletionResult{}, err
	}
	var output interface{}
	var raw []byte
	if c.Values != nil {
		raw, err = json.Marshal(c.Values)
		if err != nil {
			return state.CompletionResult{}, err
		}
		if d.ValidateValues != nil {
			if err := d.ValidateValues(ctx, tx, raw); err != nil {
				return state.CompletionResult{}, err
			}
		}
		output = raw
	}
	// Cancellation usage belongs to the run, not to conversation history.
	if c.Values != nil && !c.CancelCurrentAttempt {
		var oldRaw []byte
		if err := tx.Row(ctx, d.query(`SELECT values_json FROM threads WHERE thread_id = ? AND tenant_id = ?`+d.lock()), threadID, tid).Scan(&oldRaw); err != nil {
			return state.CompletionResult{}, err
		}
		merged := map[string]interface{}{}
		if len(oldRaw) > 0 {
			if err := jsonvalue.Unmarshal(oldRaw, &merged); err != nil {
				return state.CompletionResult{}, err
			}
		}
		if merged == nil {
			merged = map[string]interface{}{}
		}
		for k, v := range c.Values {
			merged[k] = v
		}
		threadRaw, err := json.Marshal(merged)
		if err != nil {
			return state.CompletionResult{}, err
		}
		if d.ValidateValues != nil {
			if err := d.ValidateValues(ctx, tx, threadRaw); err != nil {
				return state.CompletionResult{}, err
			}
		}
		if err := tx.Exec(ctx, d.query(`UPDATE threads SET values_json = ?, version = version + 1, updated_at = ? WHERE thread_id = ? AND tenant_id = ?`), threadRaw, d.now(now), threadID, tid); err != nil {
			return state.CompletionResult{}, err
		}
		var parentID sql.NullString
		err = tx.Row(ctx, d.query(`SELECT checkpoint_id FROM thread_checkpoints WHERE thread_id = ? AND tenant_id = ? ORDER BY created_at DESC LIMIT 1`), threadID, tid).Scan(&parentID)
		if err != nil && !isNoRows(err) {
			return state.CompletionResult{}, err
		}
		var parent interface{}
		if parentID.Valid {
			parent = parentID.String
		}
		if err := tx.Exec(ctx, d.query(`INSERT INTO thread_checkpoints (tenant_id,checkpoint_id,thread_id,checkpoint_ns,parent_id,values_json,metadata,next_nodes,tasks,interrupts,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), tid, "completion:"+c.RunID, threadID, "", parent, raw, []byte(`{}`), []byte(`[]`), []byte(`[]`), []byte(`[]`), d.now(now)); err != nil {
			return state.CompletionResult{}, err
		}
	}
	if err := tx.Exec(ctx, d.query(`UPDATE runs SET metadata = ?, status = ?, output = ?, error_msg = ?, updated_at = ? WHERE run_id = ? AND tenant_id = ?`), rawMeta, string(c.Status), output, c.Error, d.now(now), c.RunID, tid); err != nil {
		return state.CompletionResult{}, err
	}
	threadStatus := models.ThreadStatusIdle
	if c.Status == models.RunStatusInterrupted {
		threadStatus = models.ThreadStatusInterrupted
	}
	// Locking the thread makes release serialize with TryClaimThread. The
	// current run is already terminal in this transaction before the check.
	var ignored string
	if err := tx.Row(ctx, d.query(`SELECT status FROM threads WHERE thread_id = ? AND tenant_id = ?`+d.lock()), threadID, tid).Scan(&ignored); err != nil {
		return state.CompletionResult{}, err
	}
	var active int
	if err := tx.Row(ctx, d.query(`SELECT COUNT(*) FROM runs WHERE thread_id = ? AND tenant_id = ? AND status IN ('pending','running') AND run_id <> ?`), threadID, tid, c.RunID).Scan(&active); err != nil {
		return state.CompletionResult{}, err
	}
	if active == 0 {
		if err := tx.Exec(ctx, d.query(`UPDATE threads SET status = ?, updated_at = ? WHERE thread_id = ? AND tenant_id = ? AND status = 'busy'`), string(threadStatus), d.now(now), threadID, tid); err != nil {
			return state.CompletionResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return state.CompletionResult{}, err
	}
	return result, nil
}
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// DatabaseSQL adapts database/sql transactions. SQLite uses BEGIN IMMEDIATE
// so read/modify/write is serialized before the metadata is inspected.
func DatabaseSQL(db *sql.DB, dialect string) Driver {
	return Driver{Dialect: dialect, Begin: func(ctx context.Context) (Tx, error) {
		if dialect == "sqlite" {
			conn, err := db.Conn(ctx)
			if err != nil {
				return nil, err
			}
			if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
				conn.Close()
				return nil, err
			}
			return &sqliteTx{conn: conn}, nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		return sqlTx{tx}, nil
	}}
}

type sqlTx struct{ *sql.Tx }

// WrapSQLTransaction lets a store reuse the completion lock inside an existing
// database/sql transaction without opening another connection.
func WrapSQLTransaction(tx *sql.Tx) Tx { return sqlTx{tx} }

func (t sqlTx) Exec(ctx context.Context, q string, a ...interface{}) error {
	_, err := t.ExecContext(ctx, q, a...)
	return err
}
func (t sqlTx) Row(ctx context.Context, q string, a ...interface{}) Row {
	return t.QueryRowContext(ctx, q, a...)
}
func (t sqlTx) Commit(context.Context) error   { return t.Tx.Commit() }
func (t sqlTx) Rollback(context.Context) error { return t.Tx.Rollback() }

type sqliteTx struct {
	conn   *sql.Conn
	closed bool
}

func (t *sqliteTx) Exec(ctx context.Context, q string, a ...interface{}) error {
	_, err := t.conn.ExecContext(ctx, q, a...)
	return err
}
func (t *sqliteTx) Row(ctx context.Context, q string, a ...interface{}) Row {
	return t.conn.QueryRowContext(ctx, q, a...)
}
func (t *sqliteTx) Commit(ctx context.Context) error {
	if err := t.Exec(ctx, `COMMIT`); err != nil {
		return err
	}
	t.closed = true
	return t.conn.Close()
}
func (t *sqliteTx) Rollback(ctx context.Context) error {
	if t.closed {
		return nil
	}
	t.closed = true
	err := t.Exec(ctx, `ROLLBACK`)
	t.conn.Close()
	return err
}
