package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upMandatoryHITLRules(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS mandatory_hitl_rules (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent_id   TEXT NOT NULL DEFAULT '',
			connector  TEXT NOT NULL,
			tools      JSONB NOT NULL DEFAULT '[]',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (tenant_id, agent_id, connector)
		);
		CREATE INDEX IF NOT EXISTS idx_mandatory_hitl_rules_tenant ON mandatory_hitl_rules(tenant_id);
	`)
	return err
}

// ListMandatoryHITLRules returns every durable mandatory-HITL rule.
func (s *Store) ListMandatoryHITLRules(ctx context.Context) ([]*models.MandatoryHITLRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM mandatory_hitl_rules
		ORDER BY tenant_id ASC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMandatoryHITLRules(rows)
}

// SearchMandatoryHITLRules lists rules with optional filters and keyset paging.
func (s *Store) SearchMandatoryHITLRules(ctx context.Context, req *models.MandatoryHITLSearchRequest) ([]*models.MandatoryHITLRule, error) {
	if req == nil {
		req = &models.MandatoryHITLSearchRequest{}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at FROM mandatory_hitl_rules`
	var args []interface{}
	var where []string
	argN := 1
	if !tenant.IsSystem(ctx) {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, tenant.FromContext(ctx))
		argN++
	} else if req.TenantID != "" {
		where = append(where, fmt.Sprintf("tenant_id = $%d", argN))
		args = append(args, req.TenantID)
		argN++
	}
	if req.AgentID != "" {
		where = append(where, fmt.Sprintf("agent_id = $%d", argN))
		args = append(args, req.AgentID)
		argN++
	}
	if req.Connector != "" {
		where = append(where, fmt.Sprintf("connector = $%d", argN))
		args = append(args, req.Connector)
		argN++
	}
	kc, err := pagecursor.DecodeKey(req.Cursor)
	if err != nil {
		return nil, err
	}
	if kc.ID != "" {
		where = append(where, fmt.Sprintf("(tenant_id > $%d OR (tenant_id = $%d AND id > $%d))", argN, argN, argN+1))
		args = append(args, kc.Key, kc.ID)
		argN += 2
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if kc.ID != "" {
		query += fmt.Sprintf(" ORDER BY tenant_id ASC, id ASC LIMIT $%d", argN)
		args = append(args, limit)
	} else {
		query += fmt.Sprintf(" ORDER BY tenant_id ASC, id ASC LIMIT $%d OFFSET $%d", argN, argN+1)
		args = append(args, limit, req.Offset)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMandatoryHITLRules(rows)
}

// UpsertMandatoryHITLRule inserts or replaces a rule by id; enforces unique
// (tenant_id, agent_id, connector).
func (s *Store) UpsertMandatoryHITLRule(ctx context.Context, r *models.MandatoryHITLRule) error {
	if r == nil {
		return nil
	}
	tools, err := marshalHITLTools(r.Tools)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err = s.pool.Exec(ctx, `
		INSERT INTO mandatory_hitl_rules (id, tenant_id, agent_id, connector, tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			agent_id = EXCLUDED.agent_id,
			connector = EXCLUDED.connector,
			tools = EXCLUDED.tools,
			updated_at = EXCLUDED.updated_at
	`, r.ID, r.TenantID, r.AgentID, r.Connector, tools, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &state.ErrConflict{
				Resource: "mandatory_hitl_rule",
				ID:       r.TenantID + "/" + r.AgentID + "/" + r.Connector,
				Reason:   "already exists for tenant/agent/connector (use PUT on the existing id)",
			}
		}
	}
	return err
}

// GetMandatoryHITLRule returns one rule by id, or state.ErrNotFound.
func (s *Store) GetMandatoryHITLRule(ctx context.Context, id string) (*models.MandatoryHITLRule, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM mandatory_hitl_rules WHERE id = $1
	`, id)
	r, err := scanMandatoryHITLRule(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "mandatory_hitl_rule", ID: id}
		}
		return nil, err
	}
	return r, nil
}

// DeleteMandatoryHITLRule removes a rule by id. Missing id is not an error.
func (s *Store) DeleteMandatoryHITLRule(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM mandatory_hitl_rules WHERE id = $1`, id)
	return err
}

type mandatoryHITLScanner interface {
	Scan(dest ...any) error
}

func marshalHITLTools(tools []string) ([]byte, error) {
	if tools == nil {
		tools = []string{}
	}
	return json.Marshal(tools)
}

func scanMandatoryHITLRule(row mandatoryHITLScanner) (*models.MandatoryHITLRule, error) {
	var r models.MandatoryHITLRule
	var toolsRaw []byte
	if err := row.Scan(&r.ID, &r.TenantID, &r.AgentID, &r.Connector, &toolsRaw, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Tools = unmarshalHITLTools(toolsRaw)
	return &r, nil
}

func scanMandatoryHITLRules(rows pgx.Rows) ([]*models.MandatoryHITLRule, error) {
	out := []*models.MandatoryHITLRule{}
	for rows.Next() {
		r, err := scanMandatoryHITLRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func unmarshalHITLTools(raw []byte) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil || tools == nil {
		return []string{}
	}
	return tools
}
