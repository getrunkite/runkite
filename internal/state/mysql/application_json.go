package mysql

import (
	"context"
	"fmt"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

// MySQL's native JSON representation can change numbers even after Go preserves
// their tokens. Compare the server's actual conversion before writing any
// application values. The transaction hook uses its existing connection.
func validateNativeApplicationJSON(
	ctx context.Context,
	row func(context.Context, string, ...interface{}) sqlcompletion.Row,
	raw []byte,
) error {
	if len(raw) == 0 {
		return nil
	}
	var original interface{}
	if err := jsonvalue.Unmarshal(raw, &original); err != nil {
		return fmt.Errorf("decode MySQL application JSON: %w", err)
	}
	var native []byte
	if err := row(ctx, `SELECT CAST(? AS JSON)`, string(raw)).Scan(&native); err != nil {
		return fmt.Errorf("validate MySQL application JSON: %w", err)
	}
	var converted interface{}
	if err := jsonvalue.Unmarshal(native, &converted); err != nil {
		return fmt.Errorf("decode native MySQL application JSON: %w", err)
	}
	if !jsonvalue.Equal(original, converted) {
		return fmt.Errorf("MySQL JSON representation cannot preserve application numbers exactly")
	}
	return nil
}

func (s *Store) validateApplicationJSON(ctx context.Context, raw []byte) error {
	return validateNativeApplicationJSON(ctx, func(ctx context.Context, query string, args ...interface{}) sqlcompletion.Row {
		return s.db.QueryRowContext(ctx, query, args...)
	}, raw)
}

func validateCompletionApplicationJSON(ctx context.Context, tx sqlcompletion.Tx, raw []byte) error {
	return validateNativeApplicationJSON(ctx, tx.Row, raw)
}
