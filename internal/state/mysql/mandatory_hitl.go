package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/migrate"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Store) upMandatoryHITLRules(ctx context.Context, db migrate.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS mandatory_hitl_rules (
			id         VARCHAR(255) PRIMARY KEY,
			tenant_id  VARCHAR(255) NOT NULL,
			agent_id   VARCHAR(255) NOT NULL DEFAULT '',
			connector  VARCHAR(255) NOT NULL,
			tools      JSON,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY uq_mandatory_hitl_rules_tac (tenant_id, agent_id, connector),
			KEY idx_mandatory_hitl_rules_tenant (tenant_id)
		) ENGINE=InnoDB CHARSET=utf8mb4`)
	return err
}

// ListMandatoryHITLRules returns every durable mandatory-HITL rule.
func (s *Store) ListMandatoryHITLRules(ctx context.Context) ([]*models.MandatoryHITLRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM mandatory_hitl_rules ORDER BY tenant_id ASC, id ASC`)
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
	if !tenant.IsSystem(ctx) {
		where = append(where, "tenant_id = ?")
		args = append(args, tenant.FromContext(ctx))
	} else if req.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, req.TenantID)
	}
	if req.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, req.AgentID)
	}
	if req.Connector != "" {
		where = append(where, "connector = ?")
		args = append(args, req.Connector)
	}
	kc, err := pagecursor.DecodeKey(req.Cursor)
	if err != nil {
		return nil, err
	}
	if kc.ID != "" {
		where = append(where, "(tenant_id > ? OR (tenant_id = ? AND id > ?))")
		args = append(args, kc.Key, kc.Key, kc.ID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if kc.ID != "" {
		query += " ORDER BY tenant_id ASC, id ASC LIMIT ?"
		args = append(args, limit)
	} else {
		query += " ORDER BY tenant_id ASC, id ASC LIMIT ? OFFSET ?"
		args = append(args, limit, req.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMandatoryHITLRules(rows)
}

// UpsertMandatoryHITLRule inserts or replaces by id. MySQL cannot scope
// ON DUPLICATE KEY UPDATE to only the primary key, so update-by-id then
// insert maps a (tenant,agent,connector) collision to ErrConflict.
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE mandatory_hitl_rules SET tenant_id = ?, agent_id = ?, connector = ?, tools = ?, updated_at = ?
		WHERE id = ?`,
		r.TenantID, r.AgentID, r.Connector, nullableJSON(tools), r.UpdatedAt, r.ID)
	if err != nil {
		if isDuplicateKeyError(err) {
			return mandatoryHITLConflict(r)
		}
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mandatory_hitl_rules (id, tenant_id, agent_id, connector, tools, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		r.ID, r.TenantID, r.AgentID, r.Connector, nullableJSON(tools), r.CreatedAt, r.UpdatedAt)
	if isDuplicateKeyError(err) {
		return mandatoryHITLConflict(r)
	}
	return err
}

func mandatoryHITLConflict(r *models.MandatoryHITLRule) error {
	return &state.ErrConflict{
		Resource: "mandatory_hitl_rule",
		ID:       r.TenantID + "/" + r.AgentID + "/" + r.Connector,
		Reason:   "already exists for tenant/agent/connector (use PUT on the existing id)",
	}
}

// GetMandatoryHITLRule returns one rule by id.
func (s *Store) GetMandatoryHITLRule(ctx context.Context, id string) (*models.MandatoryHITLRule, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, agent_id, connector, tools, created_at, updated_at
		FROM mandatory_hitl_rules WHERE id = ?`, id)
	r, err := scanMandatoryHITLRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &state.ErrNotFound{Resource: "mandatory_hitl_rule", ID: id}
		}
		return nil, err
	}
	return r, nil
}

// DeleteMandatoryHITLRule removes a rule by id.
func (s *Store) DeleteMandatoryHITLRule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mandatory_hitl_rules WHERE id = ?`, id)
	return err
}

func marshalHITLTools(tools []string) ([]byte, error) {
	if tools == nil {
		tools = []string{}
	}
	return json.Marshal(tools)
}

func scanMandatoryHITLRule(row govScanner) (*models.MandatoryHITLRule, error) {
	var r models.MandatoryHITLRule
	var toolsRaw []byte
	if err := row.Scan(&r.ID, &r.TenantID, &r.AgentID, &r.Connector, &toolsRaw, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Tools = unmarshalHITLTools(toolsRaw)
	return &r, nil
}

func scanMandatoryHITLRules(rows *sql.Rows) ([]*models.MandatoryHITLRule, error) {
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
