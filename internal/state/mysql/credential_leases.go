package mysql

import (
	"context"

	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/state/migrate"
)

func (s *Store) CredentialLeaseStorage() (credentials.Store, error) {
	return credentials.NewSQLStore(s.db, credentials.MySQL)
}
func (s *Store) upCredentialLeases(ctx context.Context, db migrate.DB) error {
	queries, err := credentials.Schema(credentials.MySQL)
	if err != nil {
		return err
	}
	for _, q := range queries {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
