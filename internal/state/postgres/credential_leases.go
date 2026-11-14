package postgres

import (
	"context"

	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *Store) CredentialLeaseStorage() (credentials.Store, error) {
	return credentials.NewPGXStore(s.pool)
}
func (s *Store) upCredentialLeases(ctx context.Context, conn *pgxpool.Conn) error {
	queries, err := credentials.Schema(credentials.Postgres)
	if err != nil {
		return err
	}
	for _, q := range queries {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
