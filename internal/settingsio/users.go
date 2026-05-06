// Package settingsio: cross-instance user export/import.
//
// Users are exported by username (the only key that is portable across
// instances; ids are uuid-per-instance). On import, users that already exist
// on the target are left untouched -- the operator's existing setup wins
// over the envelope. Users absent on the target are recreated so that
// downstream rows (api tokens, user preferences) can attribute back to them
// without a fallback.
//
// This file lives alongside tokens.go and libraries.go to keep the per-domain
// export/import helpers physically together; the dispatch from Service.Export /
// Service.Import is in export.go.
package settingsio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// UserExport carries a single user row in a form portable across instances.
// Internal ids (id, invited_by) are intentionally omitted because they are
// regenerated per-instance; downstream rows (api_tokens, user_preferences)
// remap by username instead.
//
// PasswordHash is preserved for local-auth users so credentials survive a
// restore. The hash is a bcrypt digest, never plaintext, and it only crosses
// the wire inside the passphrase-encrypted envelope. Federated-only users
// have an empty hash (their schema row already stores ”).
type UserExport struct {
	Username     string `json:"username"`
	DisplayName  string `json:"display_name,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
	Role         string `json:"role"`
	AuthProvider string `json:"auth_provider,omitempty"`
	ProviderID   string `json:"provider_id,omitempty"`
	IsActive     bool   `json:"is_active"`
	IsProtected  bool   `json:"is_protected,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// exportUsers reads every users row in a form portable across instances.
// The bootstrap admin row is included in the dump but its is_protected flag
// is preserved; on import the target's existing protected row is what
// survives because import skips users whose username already exists.
func (s *Service) exportUsers(ctx context.Context) ([]UserExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT username, display_name, password_hash, role, auth_provider,
		       provider_id, is_active, is_protected, created_at
		FROM users
		ORDER BY created_at, username
	`)
	if err != nil {
		return nil, fmt.Errorf("querying users: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []UserExport
	for rows.Next() {
		var row UserExport
		var isActive, isProtected int
		if err := rows.Scan(
			&row.Username, &row.DisplayName, &row.PasswordHash, &row.Role,
			&row.AuthProvider, &row.ProviderID, &isActive, &isProtected, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning user row: %w", err)
		}
		row.IsActive = isActive != 0
		row.IsProtected = isProtected != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user rows: %w", err)
	}
	return out, nil
}

// importUsers inserts users from the envelope that are absent on the target,
// keyed by username. Existing users are left untouched: the operator's local
// setup (potentially with a rotated password or different role) wins over
// the envelope's snapshot. is_protected is forced to 0 on insert so the
// envelope cannot smuggle in a second protected row that would conflict
// with the target's bootstrap admin.
//
// Returns the number of inserted users so the caller can record an audit
// count; rows that already existed are not counted.
func (s *Service) importUsers(ctx context.Context, users []UserExport, result *ImportResult) error {
	if len(users) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, u := range users {
		if u.Username == "" {
			slog.Warn("import: skipping user with empty username")
			continue
		}
		// Probe for existing row first. We do not UPDATE on conflict because
		// overwriting the operator's password_hash or role with the source
		// instance's older snapshot is far worse than leaving the target's
		// row alone; the only data loss case (user does not exist on target)
		// is what this import path is designed to fix.
		var existingID string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE username = ?`, u.Username,
		).Scan(&existingID)
		if err == nil {
			// Already exists: keep target's row; do not overwrite.
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("looking up user %q: %w", u.Username, err)
		}

		role := u.Role
		if role != "administrator" && role != "operator" && role != "admin" {
			// Unknown role -- coerce to operator (least privilege). Auth
			// material must fail closed: a tampered or future-version role
			// must not silently grant Administrator.
			role = "operator"
		}
		authProvider := u.AuthProvider
		if authProvider == "" {
			authProvider = "local"
		}
		isActive := 0
		if u.IsActive {
			isActive = 1
		}
		createdAt := u.CreatedAt
		if createdAt == "" {
			createdAt = now
		}
		id := uuid.New().String()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO users (
				id, username, display_name, password_hash, role,
				auth_provider, provider_id, is_active, is_protected,
				invited_by, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?)
		`,
			id, u.Username, u.DisplayName, u.PasswordHash, role,
			authProvider, u.ProviderID, isActive,
			createdAt, now,
		); err != nil {
			return fmt.Errorf("inserting user %q: %w", u.Username, err)
		}
		result.UsersImported++
	}
	return nil
}
