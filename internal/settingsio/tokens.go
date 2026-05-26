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
// only inside the passphrase-encrypted envelope.
//
// From envelope v1.4 onward the source instance's UserID (UUID) is also
// carried so the import side can match by id first (stable across installs
// once the Users block has restored the source ids) and only fall back to
// the historical username remap when no id match exists. This covers the
// "same user, locally renamed on target" case where username-only lookup
// would silently skip every token belonging to that user. UserID is
// omitempty so pre-1.4 envelopes (which never had it) still decode and the
// importer falls back to the username path for those rows.
type APITokenExport struct {
	Name       string `json:"name"`
	TokenHash  string `json:"token_hash"`
	Scopes     string `json:"scopes"`
	UserID     string `json:"user_id,omitempty"`
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
		SELECT t.name, t.token_hash, t.scopes, u.id, u.username,
		       t.created_at, COALESCE(t.last_used_at, ''),
		       COALESCE(t.revoked_at, ''), t.status
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		ORDER BY t.created_at, t.name
	`)
	if err != nil {
		return nil, fmt.Errorf("querying api tokens: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []APITokenExport
	for rows.Next() {
		var te APITokenExport
		if err := rows.Scan(
			&te.Name, &te.TokenHash, &te.Scopes, &te.UserID, &te.Username,
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
// schema). The owning user is resolved with a two-step probe: id first
// (stable across installs once the Users block has restored source ids),
// then username (historical pre-1.4 path).
//
// Resolution order:
//  1. te.UserID present (v1.4+ envelope) AND that id exists on the target
//     -> token attributes back to that user, even if the local username
//     differs (the operator renamed the account locally).
//  2. id absent or id miss -> probe by username. If found (pre-existing OR
//     just recreated from the envelope's Users block) -> token attributes
//     to that user.
//  3. Both probes miss AND opts.AdminFallbackTokens=true -> token is
//     attributed to opts.ImportingAdminUserID and OwnershipReassigned is
//     incremented in the result so the operator can see the audit count.
//  4. Both probes miss AND admin-fallback is off (the historical default) ->
//     token is skipped and APITokensSkipped is incremented; this preserves
//     the prior behavior for callers that prefer a quiet skip over a silent
//     ownership change (e.g. prod->staging clones).
//
// The id-first path was added in #1691 because the original username-only
// lookup silently dropped every token belonging to a user who shared the
// source id but had been locally renamed on the target.
func (s *Service) importAPITokens(ctx context.Context, db dbExecutor, tokens []APITokenExport, result *ImportResult, opts ImportOptions) error {
	for i := range tokens {
		te := &tokens[i]
		if te.TokenHash == "" {
			// A blank hash cannot satisfy authentication and would collide
			// on the UNIQUE constraint after one row. Skip with a warning
			// rather than fail, matching the defensive posture used elsewhere.
			slog.Warn("import: skipping api token with empty hash", "name", te.Name)
			result.APITokensSkipped++
			continue
		}
		// Resolve owning user. id-first probe (v1.4+ envelopes) covers
		// the same-user-renamed-locally case; username fallback covers
		// pre-1.4 envelopes and id-misses. See resolveTokenOwner for
		// the exact resolution rules.
		userID, skip, err := s.resolveTokenOwner(ctx, db, te, opts, result)
		if err != nil {
			return err
		}
		if skip {
			continue
		}

		// Upsert by token_hash. We look up first (instead of using ON CONFLICT)
		// so the existing row's id is preserved and only metadata is updated;
		// re-importing the same export is therefore idempotent without
		// introducing a new row each pass.
		var existingID string
		err = db.QueryRowContext(ctx,
			`SELECT id FROM api_tokens WHERE token_hash = ?`, te.TokenHash,
		).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id := uuid.New().String()
			if _, err := db.ExecContext(ctx, `
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
			if _, err := db.ExecContext(ctx, `
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

// resolveTokenOwner returns the target user_id this token should be
// attributed to, applying the four-step resolution documented on
// importAPITokens. (skip=true, err=nil) means the loop should continue
// past this token without writing it; (skip=false, err=nil) means userID
// is bound and the caller should proceed with the upsert. A non-nil err
// is a real DB failure and must bubble up so the whole import fails fast
// rather than masking a transient outage as a per-token skip.
func (s *Service) resolveTokenOwner(ctx context.Context, db dbExecutor, te *APITokenExport, opts ImportOptions, result *ImportResult) (userID string, skip bool, err error) {
	// Step 1: id probe (v1.4+ envelopes). An id miss is not an error here
	// because the target may have the user under a different id; fall
	// through to the username probe instead of failing.
	if te.UserID != "" {
		var found string
		probeErr := db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE id = ?`, te.UserID,
		).Scan(&found)
		switch {
		case probeErr == nil:
			return found, false, nil
		case errors.Is(probeErr, sql.ErrNoRows):
			// fall through to username probe
		default:
			return "", false, fmt.Errorf("probing user by id %q for token %q: %w", te.UserID, te.Name, probeErr)
		}
	}

	// Step 2: username probe.
	var found string
	probeErr := db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = ?`, te.Username,
	).Scan(&found)
	switch {
	case probeErr == nil:
		return found, false, nil
	case errors.Is(probeErr, sql.ErrNoRows):
		// fall through to admin-fallback / skip below
	default:
		return "", false, fmt.Errorf("looking up user %q for token %q: %w", te.Username, te.Name, probeErr)
	}

	// Step 3: admin-fallback opt-in.
	if opts.AdminFallbackTokens && opts.ImportingAdminUserID != "" {
		// Verify the importing admin still exists on the target. A
		// tampered or stale opt would otherwise insert a token with a
		// dangling user_id FK.
		var adminProbe string
		probeErr := db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE id = ?`, opts.ImportingAdminUserID,
		).Scan(&adminProbe)
		switch {
		case probeErr == nil:
			slog.Info("import: reassigning api token to importing admin (admin-fallback)",
				"token_name", te.Name,
				"original_username", te.Username,
				"new_owner_id", opts.ImportingAdminUserID)
			result.OwnershipReassigned++
			return opts.ImportingAdminUserID, false, nil
		case errors.Is(probeErr, sql.ErrNoRows):
			// Genuine "admin deleted between resolution and import":
			// skip the token, do not fail the whole import.
			slog.Warn("import: admin-fallback configured but admin id missing on target; skipping token",
				"token_name", te.Name, "username", te.Username,
				"admin_id", opts.ImportingAdminUserID)
			result.APITokensSkipped++
			return "", true, nil
		default:
			// A real DB error must not be silently swallowed as "admin
			// missing"; that turns a transient outage into a quiet
			// bulk-token loss.
			return "", false, fmt.Errorf("probing importing admin %q for token fallback: %w", opts.ImportingAdminUserID, probeErr)
		}
	}

	// Step 4: skip with audit counter.
	slog.Warn("import: skipping api token whose owner is absent on target",
		"token_name", te.Name, "username", te.Username)
	result.APITokensSkipped++
	return "", true, nil
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
