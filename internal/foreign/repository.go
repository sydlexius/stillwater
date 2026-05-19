package foreign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Repository persists foreign-file ledger entries and the allowlist. It is
// intentionally small: the scanner is the only writer for foreign_files and
// the API handlers are the only writers for foreign_file_allowlist.
type Repository struct {
	db *sql.DB
}

// NewRepository wires the repository to the application *sql.DB.
func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// ErrNotFound is the sentinel returned when a delete or update affects no
// rows. Surface as 404 at the HTTP layer.
var ErrNotFound = errors.New("foreign: not found")

// Upsert records (or refreshes the detected_at on) a foreign-file entry.
// Idempotent on (artist_id, file_path) so re-scans never duplicate rows.
// content_hash may be empty for pre-008 rows; the scanner backfills it on
// the next pass via this same upsert path.
func (r *Repository) Upsert(ctx context.Context, e Entry) error {
	if e.ArtistID == "" || e.FilePath == "" || e.FileName == "" {
		return fmt.Errorf("foreign: upsert requires artist_id, file_path, file_name")
	}
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.DetectedAt.IsZero() {
		e.DetectedAt = time.Now().UTC()
	}
	var hashArg interface{}
	if e.ContentHash != "" {
		hashArg = e.ContentHash
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO foreign_files (id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_id, file_path) DO UPDATE SET
			file_name    = excluded.file_name,
			content_hash = excluded.content_hash,
			size_bytes   = excluded.size_bytes,
			detected_at  = excluded.detected_at`,
		e.ID, e.ArtistID, e.FilePath, e.FileName, hashArg, e.SizeBytes, e.DetectedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("foreign: upsert: %w", err)
	}
	return nil
}

// DeleteByPath removes a single ledger row by (artist_id, file_path).
// Used after the user takes action on a row (delete-from-disk or rescan
// proves the file is gone).
func (r *Repository) DeleteByPath(ctx context.Context, artistID, filePath string) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM foreign_files WHERE artist_id = ? AND file_path = ?`,
		artistID, filePath)
	if err != nil {
		return fmt.Errorf("foreign: delete by path: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteByID removes a single ledger row by primary key. Used by the API
// handlers when the UI sends back the row id from a list rendering.
func (r *Repository) DeleteByID(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM foreign_files WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("foreign: delete by id: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByID fetches one ledger row including the joined artist name. Returns
// ErrNotFound if no row matches.
func (r *Repository) GetByID(ctx context.Context, id string) (Entry, error) {
	var e Entry
	var detected string
	var artistName sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT f.id, f.artist_id, f.file_path, f.file_name,
		       COALESCE(f.content_hash, ''), f.size_bytes, f.detected_at,
		       a.name
		FROM foreign_files f
		LEFT JOIN artists a ON a.id = f.artist_id
		WHERE f.id = ?`, id).Scan(&e.ID, &e.ArtistID, &e.FilePath, &e.FileName, &e.ContentHash, &e.SizeBytes, &detected, &artistName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("foreign: get by id: %w", err)
	}
	if t, perr := time.Parse(time.RFC3339, detected); perr == nil {
		e.DetectedAt = t
	}
	if artistName.Valid {
		e.ArtistName = artistName.String
	}
	return e, nil
}

// List returns every ledger row joined with the owning artist name, sorted
// by artist then file path so the UI rendering is stable across reloads.
func (r *Repository) List(ctx context.Context) ([]Entry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.id, f.artist_id, f.file_path, f.file_name,
		       COALESCE(f.content_hash, ''), f.size_bytes, f.detected_at,
		       a.name
		FROM foreign_files f
		LEFT JOIN artists a ON a.id = f.artist_id
		ORDER BY a.name COLLATE NOCASE, f.file_path`)
	if err != nil {
		return nil, fmt.Errorf("foreign: list: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []Entry
	for rows.Next() {
		var e Entry
		var detected string
		var artistName sql.NullString
		if err := rows.Scan(&e.ID, &e.ArtistID, &e.FilePath, &e.FileName, &e.ContentHash, &e.SizeBytes, &detected, &artistName); err != nil {
			return nil, fmt.Errorf("foreign: scan list row: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339, detected); perr == nil {
			e.DetectedAt = t
		}
		if artistName.Valid {
			e.ArtistName = artistName.String
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("foreign: iterate list rows: %w", err)
	}
	return out, nil
}

// Count returns the number of foreign-file ledger rows. Used by the banner
// to decide whether to show the slate/blue warning.
func (r *Repository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foreign_files`).Scan(&n); err != nil {
		return 0, fmt.Errorf("foreign: count: %w", err)
	}
	return n, nil
}

// IsAllowlisted reports whether (artistID, contentHash) is suppressed from
// re-detection by either an artist-scoped or global allowlist row. Matching
// is on byte content (sha256 hex) so two distinct files sharing a basename
// no longer collide. An empty contentHash never matches; the scanner
// rehashes on demand for pre-008 rows so the input is well-formed by the
// time this is called.
func (r *Repository) IsAllowlisted(ctx context.Context, artistID, contentHash string) (bool, error) {
	if contentHash == "" {
		return false, nil
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM foreign_file_allowlist
		WHERE content_hash = ?
		  AND (scope = 'global' OR (scope = 'artist' AND artist_id = ?))`,
		contentHash, artistID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("foreign: allowlist check: %w", err)
	}
	return n > 0, nil
}

// AddAllowlist inserts an allowlist row. ArtistID must be non-empty when
// scope is "artist" and empty when scope is "global"; the writer rejects
// the inverse combinations to keep the partial unique indexes sound.
// ContentHash is required: dedupe keys on it, so an empty hash would
// silently produce duplicate rows under the partial-index WHERE clauses.
func (r *Repository) AddAllowlist(ctx context.Context, e AllowlistEntry) error {
	if e.FileName == "" {
		return fmt.Errorf("foreign: allowlist requires file_name")
	}
	if e.ContentHash == "" {
		return fmt.Errorf("foreign: allowlist requires content_hash")
	}
	switch e.Scope {
	case ScopeGlobal:
		if e.ArtistID != "" {
			return fmt.Errorf("foreign: global allowlist must not set artist_id")
		}
	case ScopeArtist:
		if e.ArtistID == "" {
			return fmt.Errorf("foreign: artist-scoped allowlist requires artist_id")
		}
	default:
		return fmt.Errorf("foreign: invalid allowlist scope %q", e.Scope)
	}
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	var artistArg interface{}
	if e.ArtistID != "" {
		artistArg = e.ArtistID
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO foreign_file_allowlist (id, scope, artist_id, file_name, content_hash, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, string(e.Scope), artistArg, strings.ToLower(e.FileName), e.ContentHash, e.Note, e.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		// Treat unique-constraint failures as benign so callers (e.g. the
		// banner Dismiss bulk action) can replay safely without surfacing
		// "duplicate" errors to the user. Match the specific SQLite phrase
		// rather than any error containing "unique" (which would absorb
		// future unrelated errors that mention a column or constraint name
		// containing the substring).
		if isUniqueConstraintErr(err) {
			return nil
		}
		return fmt.Errorf("foreign: insert allowlist: %w", err)
	}
	return nil
}

// isUniqueConstraintErr reports whether err is a SQLite UNIQUE-constraint
// violation. modernc.org/sqlite emits messages of the form
// "UNIQUE constraint failed: <table>.<col>"; this matcher pins to that
// exact phrase (substring match, lowercased) so unrelated errors that
// happen to mention "unique" (column names, comment fragments in
// driver-wrapped errors) are not silently swallowed. Lowercases the
// message before comparing to match the convention used in
// internal/auth/user.go and internal/auth/invite.go.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

// RemoveAllowlist deletes an allowlist row by id. Re-detection becomes
// possible again on the next scan.
func (r *Repository) RemoveAllowlist(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM foreign_file_allowlist WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("foreign: delete allowlist: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAllowlist returns every allowlist row sorted scope-first (global rows
// at the top), then by file_name. Joined with artists so artist-scoped rows
// can render the artist name in the management page.
func (r *Repository) ListAllowlist(ctx context.Context) ([]AllowlistEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT al.id, al.scope, COALESCE(al.artist_id, ''), al.file_name,
		       COALESCE(al.content_hash, ''), al.note, al.created_at,
		       COALESCE(a.name, '')
		FROM foreign_file_allowlist al
		LEFT JOIN artists a ON a.id = al.artist_id
		ORDER BY al.scope, al.file_name`)
	if err != nil {
		return nil, fmt.Errorf("foreign: list allowlist: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []AllowlistEntry
	for rows.Next() {
		var e AllowlistEntry
		var scope, created string
		if err := rows.Scan(&e.ID, &scope, &e.ArtistID, &e.FileName, &e.ContentHash, &e.Note, &created, &e.ArtistName); err != nil {
			return nil, fmt.Errorf("foreign: scan allowlist row: %w", err)
		}
		e.Scope = AllowlistScope(scope)
		if t, perr := time.Parse(time.RFC3339, created); perr == nil {
			e.CreatedAt = t
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("foreign: iterate allowlist rows: %w", err)
	}
	return out, nil
}
