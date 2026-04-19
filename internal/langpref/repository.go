package langpref

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrInvalid is returned by Set when the provided tag list fails
// validation. Callers that render HTTP errors should translate this to
// HTTP 400.
var ErrInvalid = errors.New("langpref: invalid language tag list")

// Repository persists and retrieves a user's ordered metadata language
// preference list from the user_preferences table.
//
// The repository does not own the database; callers construct it with
// an already-opened *sql.DB so it can participate in the same connection
// pool as the rest of the application.
type Repository struct {
	db *sql.DB
}

// NewRepository constructs a Repository backed by db. The caller retains
// ownership of db.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Get returns the ordered language preference list for userID. If no row
// is stored, or if the stored value fails validation, the default list
// ({"en"}) is returned. The returned slice is a fresh copy; the caller
// may mutate it freely.
//
// An error is returned only for unexpected database failures. A missing
// row or a malformed stored value are both treated as "no preference
// stored" and return the default.
func (r *Repository) Get(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return DefaultTags(), nil
	}

	var raw string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`,
		userID, PreferenceKey,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultTags(), nil
		}
		return nil, fmt.Errorf("langpref: querying preference for user %s: %w", userID, err)
	}

	// Even if the stored JSON is malformed, return the default rather
	// than an error. This mirrors the read-path robustness used
	// throughout the preferences layer: a single bad row should never
	// block metadata fetches.
	if _, parsed, ok := ValidateJSON(raw); ok {
		return parsed, nil
	}
	return DefaultTags(), nil
}

// Set persists the given ordered language preference list for userID.
// The tags are validated and canonicalized before writing. Order is
// preserved exactly; duplicates (case-insensitive) are rejected.
//
// Returns ErrInvalid when tags fail validation. Returns any database
// error wrapped with context on other failures.
func (r *Repository) Set(ctx context.Context, userID string, tags []string) error {
	if userID == "" {
		return fmt.Errorf("langpref: user id is required")
	}
	canonical, ok := Validate(tags)
	if !ok {
		return ErrInvalid
	}
	encoded, err := EncodeJSON(canonical)
	if err != nil {
		// Validate already guarantees JSON-encodable content, so this
		// is a belt-and-braces check.
		return fmt.Errorf("langpref: encoding validated tags: %w", err)
	}

	_, err = r.db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET
		     value = excluded.value,
		     updated_at = excluded.updated_at`,
		userID, PreferenceKey, encoded,
	)
	if err != nil {
		return fmt.Errorf("langpref: writing preference for user %s: %w", userID, err)
	}
	return nil
}

// EffectiveForBackground returns the preference list that background jobs
// (e.g. the rule scheduler) should use when no HTTP user session is in
// scope. The strategy is deterministic and small:
//
//  1. Pick the oldest active administrator. Protected administrators
//     (bootstrap accounts that cannot be deactivated) win ties -- in a
//     single-admin self-hosted deployment this is always the operator.
//  2. Return that user's stored preferences via Get, which falls back to
//     DefaultTags() when the row is absent or malformed.
//  3. If no administrator row exists (fresh database, before bootstrap),
//     return DefaultTags().
//
// A DB error returning the candidate user row is not fatal: the function
// falls back to DefaultTags() so a transient failure never stalls a
// scheduled rule evaluation. The same is true of Get errors on the found
// user.
func (r *Repository) EffectiveForBackground(ctx context.Context) []string {
	var userID string
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM users
		WHERE role = 'administrator' AND is_active = 1
		ORDER BY is_protected DESC, created_at ASC, id ASC
		LIMIT 1
	`).Scan(&userID)
	if err != nil || userID == "" {
		return DefaultTags()
	}
	prefs, err := r.Get(ctx, userID)
	if err != nil || len(prefs) == 0 {
		return DefaultTags()
	}
	return prefs
}

// GetRaw returns the raw JSON value stored for userID, or DefaultJSON
// when no row exists or the stored value is malformed. This is the
// JSON-string counterpart to Get. It is convenient for HTTP handlers
// that pass the value straight back to the client without reparsing.
func (r *Repository) GetRaw(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return DefaultJSON, nil
	}
	var raw string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`,
		userID, PreferenceKey,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultJSON, nil
		}
		return "", fmt.Errorf("langpref: querying preference for user %s: %w", userID, err)
	}
	return NormalizeJSON(raw), nil
}
