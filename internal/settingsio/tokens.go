package settingsio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// APITokenExport carries the persisted API-token row in a form portable
// across instances. The plaintext token is never persisted in the DB, so
// this struct cannot expose it; only the stored hash crosses the wire, and
// only inside the passphrase-encrypted envelope. Username replaces user_id
// because user IDs are generated per-instance (same remap pattern as
// user_preferences).
type APITokenExport struct {
	Name       string `json:"name"`
	TokenHash  string `json:"token_hash"`
	Scopes     string `json:"scopes"`
	Username   string `json:"username"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	Status     string `json:"status"`
}

// exportAPITokens reads every api_tokens row joined to its owner's username
// so the import step can remap user_id on the target. Revoked tokens are
// included as-is (status + revoked_at preserved) so a revoke applied on the
// source survives the round-trip rather than being silently re-activated.
func (s *Service) exportAPITokens(ctx context.Context) ([]APITokenExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.name, t.token_hash, t.scopes, u.username,
		       t.created_at, COALESCE(t.last_used_at, ''),
		       COALESCE(t.revoked_at, ''), t.status
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		ORDER BY t.created_at, t.name
	`)
	if err != nil {
		return nil, fmt.Errorf("querying api tokens: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []APITokenExport
	for rows.Next() {
		var te APITokenExport
		if err := rows.Scan(
			&te.Name, &te.TokenHash, &te.Scopes, &te.Username,
			&te.CreatedAt, &te.LastUsedAt, &te.RevokedAt, &te.Status,
		); err != nil {
			return nil, fmt.Errorf("scanning api token row: %w", err)
		}
		out = append(out, te)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating api token rows: %w", err)
	}
	return out, nil
}

// importAPITokens upserts API tokens by token_hash (which is UNIQUE in the
// schema). The owning user is resolved by username on the target instance.
//
// Resolution order (#1283):
//  1. Username found on target (either pre-existing OR just recreated from
//     the envelope's Users block) -> token attributes back to that user.
//  2. Username absent AND opts.AdminFallbackTokens=true -> token is
//     attributed to opts.ImportingAdminUserID and OwnershipReassigned is
//     incremented in the result so the operator can see the audit count.
//  3. Username absent AND admin-fallback is off (the historical default) ->
//     token is skipped and APITokensSkipped is incremented; this preserves
//     the prior behavior for callers that prefer a quiet skip over a silent
//     ownership change (e.g. prod->staging clones).
func (s *Service) importAPITokens(ctx context.Context, tokens []APITokenExport, result *ImportResult, opts ImportOptions) error {
	for _, te := range tokens {
		if te.TokenHash == "" {
			// A blank hash cannot satisfy authentication and would collide
			// on the UNIQUE constraint after one row. Skip with a warning
			// rather than fail, matching the defensive posture used elsewhere.
			slog.Warn("import: skipping api token with empty hash", "name", te.Name)
			result.APITokensSkipped++
			continue
		}
		// Resolve owning user by username.
		var userID string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE username = ?`, te.Username,
		).Scan(&userID)
		switch {
		case err == nil:
			// Owner found (pre-existing or just recreated from envelope).
		case errors.Is(err, sql.ErrNoRows):
			if opts.AdminFallbackTokens && opts.ImportingAdminUserID != "" {
				// Verify the importing admin still exists on the target.
				// A tampered or stale opt would otherwise insert a token
				// with a dangling user_id FK.
				var adminProbe string
				probeErr := s.db.QueryRowContext(ctx,
					`SELECT id FROM users WHERE id = ?`, opts.ImportingAdminUserID,
				).Scan(&adminProbe)
				if probeErr != nil {
					slog.Warn("import: admin-fallback configured but admin id missing on target; skipping token",
						"token_name", te.Name, "username", te.Username,
						"admin_id", opts.ImportingAdminUserID, "probe_error", probeErr)
					result.APITokensSkipped++
					continue
				}
				slog.Info("import: reassigning api token to importing admin (admin-fallback)",
					"token_name", te.Name,
					"original_username", te.Username,
					"new_owner_id", opts.ImportingAdminUserID)
				userID = opts.ImportingAdminUserID
				result.OwnershipReassigned++
			} else {
				slog.Warn("import: skipping api token whose owner is absent on target",
					"token_name", te.Name, "username", te.Username)
				result.APITokensSkipped++
				continue
			}
		default:
			return fmt.Errorf("looking up user %q for token %q: %w", te.Username, te.Name, err)
		}

		// Upsert by token_hash. We look up first (instead of using ON CONFLICT)
		// so the existing row's id is preserved and only metadata is updated;
		// re-importing the same export is therefore idempotent without
		// introducing a new row each pass.
		var existingID string
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM api_tokens WHERE token_hash = ?`, te.TokenHash,
		).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id := uuid.New().String()
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO api_tokens (
					id, name, token_hash, scopes, user_id,
					created_at, last_used_at, revoked_at, status
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				id, te.Name, te.TokenHash, validTokenScopes(te.Scopes), userID,
				te.CreatedAt,
				dbutil.NullableString(te.LastUsedAt),
				dbutil.NullableString(te.RevokedAt),
				validTokenStatus(te.Status),
			); err != nil {
				return fmt.Errorf("inserting api token %q: %w", te.Name, err)
			}
		case err != nil:
			return fmt.Errorf("looking up api token by hash: %w", err)
		default:
			// Restore created_at on conflict update so the exported audit
			// metadata is preserved when the same token_hash already exists
			// on the target. Without this the local row keeps its original
			// timestamp, drifting from the source instance's record.
			if _, err := s.db.ExecContext(ctx, `
				UPDATE api_tokens SET
					name = ?, scopes = ?, user_id = ?, created_at = ?,
					last_used_at = ?, revoked_at = ?, status = ?
				WHERE id = ?
			`,
				te.Name, validTokenScopes(te.Scopes), userID, te.CreatedAt,
				dbutil.NullableString(te.LastUsedAt),
				dbutil.NullableString(te.RevokedAt),
				validTokenStatus(te.Status),
				existingID,
			); err != nil {
				return fmt.Errorf("updating api token %q: %w", te.Name, err)
			}
		}
		result.APITokens++
	}
	return nil
}

// validTokenScopes falls back to the schema default when the import payload
// carries an empty scope string. The schema's NOT NULL DEFAULT 'read,write'
// would not apply on an explicit "" value, so we apply the same default here.
func validTokenScopes(s string) string {
	if s == "" {
		return "read,write"
	}
	return s
}

// validTokenStatus normalizes the status field to one of the two recognized
// values. Auth material must fail closed: an unknown status (tampered payload,
// a legacy value like "archived" from an older schema, or a future-version
// status this build doesn't recognize) defaults to "revoked" so the imported
// token cannot authenticate until an operator explicitly re-enables it.
func validTokenStatus(s string) string {
	switch s {
	case "active", "revoked":
		return s
	default:
		return "revoked"
	}
}
