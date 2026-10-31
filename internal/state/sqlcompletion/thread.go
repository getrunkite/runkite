package sqlcompletion

import (
	"context"
	"database/sql"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// LockThread binds a child mutation to the current parent owner. The row lock
// must be retained through the child write: a preceding API lookup cannot fence
// deletion and reuse of the same custom thread ID by a different tenant.
func (d Driver) LockThread(ctx context.Context, tx Tx, threadID string) (models.ThreadStatus, error) {
	var status models.ThreadStatus
	err := tx.Row(ctx, d.query(`SELECT status FROM threads WHERE thread_id = ? AND tenant_id = ?`+d.lock()), threadID, tenant.FromContext(ctx)).Scan(&status)
	if isNoRows(err) {
		return "", &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	return status, err
}

func (d Driver) WithThread(ctx context.Context, threadID string, mutate func(Tx) error) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := d.LockThread(ctx, tx, threadID); err != nil {
		return err
	}
	if err := mutate(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WrapSQLiteConnection adapts an already-open BEGIN IMMEDIATE transaction.
func WrapSQLiteConnection(conn *sql.Conn) Tx { return &sqliteTx{conn: conn} }
