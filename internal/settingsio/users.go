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
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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
	if result == nil {
		// Counter writes below dereference result unconditionally; reject a
		// nil caller up front rather than panicking partway through the
		// import (which would leave the DB in a partially-applied state).
		return errors.New("importUsers requires non-nil ImportResult")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, u := range users {
		if u.Username == "" {
			slog.Warn("import: skipping user with empty username")
			continue
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
		// INSERT OR IGNORE: a probe-then-insert flow would race against a
		// concurrent import or interactive create on the same username,
		// failing the whole import with a UNIQUE-violation when the
		// intended semantic is "if the row already exists, leave it
		// alone." The IGNORE branch matches the prior probe behavior
		// (do not overwrite the operator's password_hash or role), and
		// gating UsersImported++ on RowsAffected() == 1 keeps the audit
		// counter honest when a concurrent insert wins the race.
		res, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO users (
				id, username, display_name, password_hash, role,
				auth_provider, provider_id, is_active, is_protected,
				invited_by, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?)
		`,
			id, u.Username, u.DisplayName, u.PasswordHash, role,
			authProvider, u.ProviderID, isActive,
			createdAt, now,
		)
		if err != nil {
			return fmt.Errorf("inserting user %q: %w", u.Username, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading insert result for user %q: %w", u.Username, err)
		}
		if affected == 1 {
			result.UsersImported++
		}
	}
	return nil
}
