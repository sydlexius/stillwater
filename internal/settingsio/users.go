// Package settingsio: cross-instance user export/import.
//
// Users are exported with their UUID id so cross-install references in
// downstream rows (api tokens, user preferences) survive a restore without
// a username-based remap. On import we probe by id first: an id hit drives
// an UPDATE of the mutable fields (narrowed on protected target rows so
// the prevent_role_change trigger does not fire), an id miss followed by a
// username collision under a different id returns ErrUserIDCollision so
// the import halts rather than silently remapping, and a fresh
// (id, username) pair falls through to an INSERT carrying the source id.
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
//
// ID is the source instance's UUID. We carry it across the wire so that
// downstream rows that reference users by id (user_preferences, api_tokens)
// can keep their references intact through a restore. On the target side
// the importer probes by id first: a hit drives an UPDATE; a miss with a
// colliding username under a different id is treated as a fatal import
// error so we never silently remap one operator's account onto another;
// a clean miss falls through to an INSERT that carries the source id.
//
// PasswordHash is preserved for local-auth users so credentials survive a
// restore. The hash is a bcrypt digest, never plaintext, and only crosses
// the wire inside the passphrase-encrypted envelope. Federated-only users
// have an empty hash (their schema row already stores the empty string).
//
// AvatarURL is intentionally not yet present in the users table; reserved
// for forward-compat. Session and remember-me tokens are NEVER exported --
// they live in the sessions table which is not touched by export.
type UserExport struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
	Role         string `json:"role"`
	AuthProvider string `json:"auth_provider,omitempty"`
	ProviderID   string `json:"provider_id,omitempty"`
	IsActive     bool   `json:"is_active"`
	IsProtected  bool   `json:"is_protected,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// ErrUserIDCollision is returned by importUsers when an envelope user's
// username already exists on the target under a different id. The import
// halts so we never silently overwrite one operator's account with another.
var ErrUserIDCollision = errors.New("import: username collision under a different id")

// exportUsers reads every users row in a form portable across instances.
// The bootstrap admin's is_protected flag is preserved so the operator's
// audit trail shows the row as protected on the source.
func (s *Service) exportUsers(ctx context.Context) ([]UserExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, password_hash, role, auth_provider,
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
			&row.ID, &row.Username, &row.DisplayName, &row.PasswordHash, &row.Role,
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

// importUsers applies envelope user rows. Match is by id (stable across
// installs because the source UUID is preserved). Each row dispatches to
// importOneUser which probes SELECT-then-UPDATE rather than using
// INSERT-OR-REPLACE so the prevent_role_change trigger on protected rows
// can be honored by issuing a narrower UPDATE. Three cases:
//
//  1. A user with this id exists on the target -> UPDATE the mutable
//     fields (narrowed to display_name + password_hash + auth on a
//     protected target row so the trigger does not fire). We never
//     overwrite is_protected from the envelope; protected status is a
//     per-install policy.
//  2. No id match, but the username exists under a DIFFERENT id ->
//     ErrUserIDCollision when the envelope carried an explicit id (v1.4+).
//     The import halts; the operator must resolve the conflict manually
//     (e.g. rename one of the colliding accounts). When the envelope did
//     not carry an id (pre-1.4 envelopes), the importer cannot prove
//     identity by id and falls back to the original INSERT OR IGNORE
//     semantics: the target row wins, the envelope row is skipped with an
//     Info log, and the import continues.
//  3. Neither id nor username matches -> INSERT a new row with the source id
//     so downstream rows can still resolve owners by id.
//
// is_protected is forced to 0 on insert when there is already a protected
// row on the target; this preserves the bootstrap-admin invariant (exactly
// one protected row) while letting a fresh install accept a protected admin
// from the envelope.
func (s *Service) importUsers(ctx context.Context, db dbExecutor, users []UserExport, result *ImportResult) error {
	if len(users) == 0 {
		return nil
	}
	// importOneUser dereferences result; a nil here is a programming
	// error from the orchestrator, not user input, so fail loudly.
	if result == nil {
		return errors.New("importUsers requires non-nil ImportResult")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// If a protected admin already exists on the target, every envelope
	// row that claims is_protected=1 lands with is_protected=0 to avoid
	// violating the single-protected-admin invariant.
	var protectedCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE is_protected = 1`).Scan(&protectedCount); err != nil {
		return fmt.Errorf("counting protected users: %w", err)
	}
	hasProtected := protectedCount > 0

	for i := range users {
		if err := s.importOneUser(ctx, db, &users[i], now, &hasProtected, result); err != nil {
			return err
		}
	}
	return nil
}

// importOneUser processes a single envelope user row, applying the three
// cases (id match, username collision, fresh insert) documented on
// importUsers. hasProtected is shared mutable state across the batch so a
// freshly-inserted protected row prevents subsequent rows from also
// landing as protected, preserving the single-protected-admin invariant.
func (s *Service) importOneUser(ctx context.Context, db dbExecutor, u *UserExport, now string, hasProtected *bool, result *ImportResult) error {
	if u.Username == "" {
		slog.Warn("import: skipping user with empty username")
		return nil
	}
	// pre-id envelope versions (v1.3 and earlier) lack a UUID; synthesize
	// one so downstream rows that reference users by id still resolve.
	// Track that we synthesized it so the username-collision branch below
	// can fall back to the pre-1.4 INSERT OR IGNORE skip semantics rather
	// than failing the whole import with ErrUserIDCollision.
	sourceID := u.ID
	synthesizedID := false
	if sourceID == "" {
		sourceID = uuid.New().String()
		synthesizedID = true
	}

	role := normalizeImportRole(u.Role)
	authProvider := u.AuthProvider
	if authProvider == "" {
		authProvider = "local"
	}
	isActive := 0
	if u.IsActive {
		isActive = 1
	}
	isProtected := 0
	if u.IsProtected && !*hasProtected {
		isProtected = 1
	} else if u.IsProtected && *hasProtected {
		// The envelope brought a protected admin but the target already
		// has one. Clamping to is_protected=0 preserves the
		// single-protected-admin invariant; surface the demotion at Info
		// so the operator's audit trail shows why the restored row is
		// not protected on the target instance.
		slog.Info("import: demoting envelope-protected user; single-protected invariant",
			"username", u.Username)
	}
	createdAt := u.CreatedAt
	if createdAt == "" {
		createdAt = now
	}

	var existingByID string
	err := db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = ?`, sourceID).Scan(&existingByID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("probing user by id %q: %w", sourceID, err)
	}
	if existingByID != "" {
		// Before issuing the UPDATE, verify the envelope's username does
		// not collide with a DIFFERENT target row. If it does, the
		// UPDATE will rename the id-hit row to a username already taken
		// by another user, the schema's UNIQUE(username) trips, and the
		// caller surfaces a generic 500 instead of the documented 409
		// ErrUserIDCollision. Probing here keeps the
		// "halt on rename-into-collision" contract intact.
		var usernameOwner string
		err := db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE username = ?`, u.Username).Scan(&usernameOwner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("probing user by username %q: %w", u.Username, err)
		}
		if usernameOwner != "" && usernameOwner != sourceID {
			return fmt.Errorf("%w: username %q is taken by user id %q on target, envelope brought id %q",
				ErrUserIDCollision, u.Username, usernameOwner, sourceID)
		}
		if err := s.updateUserByID(ctx, db, u, sourceID, now, role, authProvider, isActive); err != nil {
			return err
		}
		// Do not increment UsersImported here. The counter is documented
		// on ImportResult as "user rows recreated from the envelope on
		// import because they were absent on the target"; an id-hit
		// update path is a refresh of an existing row, not a recreation,
		// so counting it would overreport the cross-instance restore on
		// any subsequent import to the same target.
		return nil
	}

	var collidingID string
	err = db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = ?`, u.Username).Scan(&collidingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("probing user by username %q: %w", u.Username, err)
	}
	if collidingID != "" {
		if synthesizedID {
			// Pre-1.4 envelope: the source carried no UUID to match
			// against, and the target already has a row with this
			// username under its own UUID. Old INSERT OR IGNORE
			// semantics: skip silently rather than fail the whole
			// import. Log at Info so the audit trail shows why the
			// envelope row did not land on the target.
			slog.Info("import: skipping pre-1.4 user; username already present on target",
				"username", u.Username, "target_id", collidingID)
			return nil
		}
		return fmt.Errorf("%w: username %q is taken by user id %q on target, envelope brought id %q",
			ErrUserIDCollision, u.Username, collidingID, sourceID)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (
			id, username, display_name, password_hash, role,
			auth_provider, provider_id, is_active, is_protected,
			invited_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
	`,
		sourceID, u.Username, u.DisplayName, u.PasswordHash, role,
		authProvider, u.ProviderID, isActive, isProtected,
		createdAt, now,
	); err != nil {
		return fmt.Errorf("inserting user %q: %w", u.Username, err)
	}
	if isProtected == 1 {
		*hasProtected = true
	}
	result.UsersImported++
	return nil
}

// updateUserByID issues either the narrow (protected target) or full
// UPDATE. The schema's prevent_role_change_protected_user trigger aborts
// any UPDATE that touches role on a protected row, so when the target row
// is protected we issue a narrower UPDATE that leaves role + is_active
// alone. Identical-value UPDATE statements still fire the trigger.
func (s *Service) updateUserByID(ctx context.Context, db dbExecutor, u *UserExport, sourceID, now, role, authProvider string, isActive int) error {
	var targetProtected int
	if err := db.QueryRowContext(ctx,
		`SELECT is_protected FROM users WHERE id = ?`, sourceID).Scan(&targetProtected); err != nil {
		return fmt.Errorf("reading is_protected for user %q: %w", u.Username, err)
	}
	if targetProtected == 1 {
		if _, err := db.ExecContext(ctx, `
			UPDATE users SET
				display_name  = ?,
				password_hash = ?,
				auth_provider = ?,
				provider_id   = ?,
				updated_at    = ?
			WHERE id = ?
		`,
			u.DisplayName, u.PasswordHash, authProvider, u.ProviderID, now, sourceID,
		); err != nil {
			return fmt.Errorf("updating protected user %q by id: %w", u.Username, err)
		}
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE users SET
			username      = ?,
			display_name  = ?,
			password_hash = ?,
			role          = ?,
			auth_provider = ?,
			provider_id   = ?,
			is_active     = ?,
			updated_at    = ?
		WHERE id = ?
	`,
		u.Username, u.DisplayName, u.PasswordHash, role,
		authProvider, u.ProviderID, isActive, now, sourceID,
	); err != nil {
		return fmt.Errorf("updating user %q by id: %w", u.Username, err)
	}
	return nil
}

// normalizeImportRole fails closed to operator (least privilege) so a
// tampered or future-version role cannot silently grant Administrator.
// "admin" is folded onto the canonical "administrator" because the rest
// of the schema only knows the long form; the short alias is a
// pre-canonical historical envelope quirk that would otherwise round-trip
// as-is and fail downstream role-FK / role-constraint checks.
func normalizeImportRole(role string) string {
	switch role {
	case "administrator", "admin":
		return "administrator"
	case "operator":
		return "operator"
	default:
		return "operator"
	}
}
