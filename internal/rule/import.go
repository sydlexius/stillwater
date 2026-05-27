package rule

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of GetByID/Update but accept a
// DBExecutor so the orchestrator can run them inside its own transaction; a
// mid-import failure then rolls back the rule writes alongside every other
// section's writes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware rule import
// helpers. Both *sql.DB and *sql.Tx satisfy it.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportGetByIDTx is the tx-aware equivalent of GetByID.
func (s *Service) ImportGetByIDTx(ctx context.Context, db DBExecutor, id string) (*Rule, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, description, category, enabled, automation_mode, config, created_at, updated_at
		FROM rules WHERE id = ?
	`, id)
	r, err := scanRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("getting rule by id: %w", err)
	}
	r.FilesystemDependent = filesystemRules[r.ID]
	return r, nil
}

// ImportUpdateTx is the tx-aware equivalent of Update. Mirrors Update's
// behavior including the soft-resolve cleanup for disabled rules so a tx-
// rolled-back import does not orphan rule_results rows.
func (s *Service) ImportUpdateTx(ctx context.Context, db DBExecutor, r *Rule) error {
	r.UpdatedAt = s.clock.Now()
	_, err := db.ExecContext(ctx, `
		UPDATE rules SET enabled = ?, automation_mode = ?, config = ?, updated_at = ? WHERE id = ?
	`, dbutil.BoolToInt(r.Enabled), r.AutomationMode, MarshalConfig(r.Config),
		r.UpdatedAt.Format(time.RFC3339), r.ID)
	if err != nil {
		return fmt.Errorf("updating rule: %w", err)
	}
	if !r.Enabled {
		if err := s.cleanupDisabledRuleStateTx(ctx, db, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// cleanupDisabledRuleStateTx mirrors cleanupDisabledRuleState but writes
// through the supplied executor.
func (s *Service) cleanupDisabledRuleStateTx(ctx context.Context, db DBExecutor, ruleID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`UPDATE rule_violations
		    SET status = ?, resolved_at = ?, updated_at = ?
		  WHERE rule_id = ? AND status IN (?, ?)`,
		ViolationStatusResolved, now, now,
		ruleID, ViolationStatusOpen, ViolationStatusPendingChoice,
	); err != nil {
		return fmt.Errorf("cleaning up violations after disable: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM rule_results WHERE rule_id = ?`, ruleID,
	); err != nil {
		return fmt.Errorf("cleaning up rule_results after disable: %w", err)
	}
	return nil
}
