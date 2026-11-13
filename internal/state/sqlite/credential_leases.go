package sqlite

import (
	"context"

	"github.com/getrunkite/runkite/internal/credentials"
)

func (s *SQLiteStore) CredentialLeaseStorage() (credentials.Store, error) {
	return credentials.NewSQLStore(s.db, credentials.SQLite)
}
func (s *SQLiteStore) upCredentialLeases(ctx context.Context) error {
	queries, err := credentials.Schema(credentials.SQLite)
	if err != nil {
		return err
	}
	for _, q := range queries {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
