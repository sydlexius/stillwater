package artist

// duplicates_ignore.go -- server-side persistence and filtering for ignored
// near-duplicate groups (#2219).
//
// The duplicates report lets an admin "ignore" a suspected-duplicate group so
// it stops surfacing in the page and the sidebar count. Before #2219 that state
// lived only in the browser's localStorage; now it is a durable server-side
// ledger (the ignored_duplicate_groups table, migration 018).
//
// A group is identified by its SIGNATURE: the member artist IDs sorted ascending
// and joined with "|". The signature is an EXACT-match key -- if the detector
// later regroups the same artists differently (a member added, removed, or
// merged away) the new group has a different signature and resurfaces for
// review rather than staying silently suppressed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ErrIgnoredGroupNotFound is returned by RestoreDuplicateGroup when no ignored
// row matches the given id. Callers map it to a 404 rather than a 500 so a
// double-restore (the row already gone) reports "not found" instead of a
// server error. Mirrors foreign.ErrNotFound in the allowlist manager.
var ErrIgnoredGroupNotFound = errors.New("ignored duplicate group not found")

// IgnoredDuplicateGroup is one persisted ignore row, surfaced to the
// manage-ignored view. Signature is the canonical identity (see
// DuplicateGroupSignature); GroupKey and Reason are the non-authoritative
// display context captured at ignore time; CreatedAt is the raw SQLite
// datetime string ("YYYY-MM-DD HH:MM:SS") from the DEFAULT (datetime('now'))
// column, passed through untouched for display.
type IgnoredDuplicateGroup struct {
	ID        string
	Signature string
	GroupKey  string
	Reason    string
	CreatedAt string
}

// DuplicateGroupSignature computes the canonical, order-invariant signature for
// a near-duplicate group from its member artist IDs: IDs are trimmed of
// surrounding whitespace, deduplicated, and the non-empty survivors sorted
// ascending and joined with "|". This matches the detector's member set and the
// legacy client key scheme exactly, so a group ignored via the API and the same
// group detected server-side produce identical signatures. Returns "" when no
// non-empty IDs remain, which callers treat as an invalid (un-ignorable) group.
func DuplicateGroupSignature(memberIDs []string) string {
	ids := make([]string, 0, len(memberIDs))
	seen := make(map[string]struct{}, len(memberIDs))
	for _, id := range memberIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return strings.Join(ids, "|")
}

// groupSignature computes the signature of a detected NearDuplicateGroup from
// its members. Thin wrapper over DuplicateGroupSignature used by the filter.
func groupSignature(g NearDuplicateGroup) string {
	ids := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		ids = append(ids, m.ID)
	}
	return DuplicateGroupSignature(ids)
}

// FilterIgnoredGroups returns the subset of groups whose signature is NOT in the
// ignored set. It is pure and database-free so it is directly unit-testable and
// so the single filter serves both the sidebar count and the page list (they
// can never diverge). Matching is EXACT on the full member signature per the
// migration's drift semantics. An empty ignored set returns the input unchanged.
func FilterIgnoredGroups(groups []NearDuplicateGroup, ignored map[string]struct{}) []NearDuplicateGroup {
	if len(ignored) == 0 {
		return groups
	}
	out := make([]NearDuplicateGroup, 0, len(groups))
	for _, g := range groups {
		if _, skip := ignored[groupSignature(g)]; skip {
			continue
		}
		out = append(out, g)
	}
	return out
}

// IgnoreDuplicateGroup persists an ignore for the group identified by signature.
// The insert is idempotent via ON CONFLICT(signature) DO NOTHING, so re-ignoring
// the same group is a benign no-op. It generates its own TEXT primary key with
// uuid.New().String(), mirroring foreign.Repository.AddAllowlist. groupKey and
// reason are stored as non-authoritative display context for the manage-ignored
// view and never participate in the match. A nil db or empty signature is a
// programming error and returns an error rather than silently succeeding.
func IgnoreDuplicateGroup(ctx context.Context, db *sql.DB, signature, groupKey, reason string) error {
	if db == nil {
		return fmt.Errorf("ignoring duplicate group: nil db")
	}
	if signature == "" {
		return fmt.Errorf("ignoring duplicate group: empty signature")
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO ignored_duplicate_groups (id, signature, group_key, reason)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(signature) DO NOTHING`,
		uuid.New().String(), signature, groupKey, reason)
	if err != nil {
		return fmt.Errorf("ignoring duplicate group: %w", err)
	}
	return nil
}

// LoadIgnoredSignatures returns the set of ignored group signatures for the pure
// filter. A nil db returns an empty set (preserving the detection code's nil-db
// test seam) rather than an error, so a partially wired Router degrades to
// "nothing ignored" instead of failing the page.
func LoadIgnoredSignatures(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if db == nil {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT signature FROM ignored_duplicate_groups`)
	if err != nil {
		return nil, fmt.Errorf("loading ignored signatures: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			return nil, fmt.Errorf("scanning ignored signature: %w", err)
		}
		out[sig] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ignored signatures: %w", err)
	}
	return out, nil
}

// LoadIgnoredGroups returns every ignored group row (full columns), newest
// first, for the manage-ignored view. Distinct from LoadIgnoredSignatures,
// which returns only the signature set for the pure filter: the manage view
// needs the id (to target a restore), the display context, and the timestamp.
// A nil db returns an empty slice (matching LoadIgnoredSignatures' nil-db seam)
// so a partially wired Router renders the empty state rather than failing.
func LoadIgnoredGroups(ctx context.Context, db *sql.DB) ([]IgnoredDuplicateGroup, error) {
	if db == nil {
		return []IgnoredDuplicateGroup{}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, signature, group_key, reason, created_at
		FROM ignored_duplicate_groups
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("loading ignored groups: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	out := make([]IgnoredDuplicateGroup, 0)
	for rows.Next() {
		var g IgnoredDuplicateGroup
		if err := rows.Scan(&g.ID, &g.Signature, &g.GroupKey, &g.Reason, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning ignored group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ignored groups: %w", err)
	}
	return out, nil
}

// RestoreDuplicateGroup removes the ignore row identified by its TEXT primary
// key id, so the group resurfaces on the next detection in both the page list
// and the sidebar count (the shared filter reads the table fresh). Returns
// ErrIgnoredGroupNotFound when no row matched, so a double-restore reports 404
// rather than silently succeeding. A nil db or empty id is a programming error.
func RestoreDuplicateGroup(ctx context.Context, db *sql.DB, id string) error {
	if db == nil {
		return fmt.Errorf("restoring duplicate group: nil db")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("restoring duplicate group: empty id")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM ignored_duplicate_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("restoring duplicate group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("restoring duplicate group: rows affected: %w", err)
	}
	if n == 0 {
		return ErrIgnoredGroupNotFound
	}
	return nil
}

// MemberCount returns the number of member artist IDs encoded in the group's
// signature (the "|"-joined member set). Used by the manage-ignored view to
// show how many records the ignored group spanned without exposing the raw
// internal IDs. Returns 0 for an empty signature.
func (g IgnoredDuplicateGroup) MemberCount() int {
	if g.Signature == "" {
		return 0
	}
	return strings.Count(g.Signature, "|") + 1
}
