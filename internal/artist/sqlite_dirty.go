package artist

import (
	"context"
	"fmt"
	"time"
)

// MarkDirty stamps dirty_since for a single artist, indicating that some
// mutation may have invalidated the artist's last rule evaluation. The
// timestamp is set to ts (UTC). updated_at is intentionally not bumped here:
// dirty tracking is bookkeeping for the rule pipeline and should not perturb
// "last modified" semantics consumed elsewhere.
func (r *sqliteArtistRepo) MarkDirty(ctx context.Context, id string, ts time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE artists SET dirty_since = ? WHERE id = ?`,
		ts.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("marking artist dirty: %w", err)
	}
	return nil
}

// MarkAllDirty stamps dirty_since on every non-excluded, non-locked artist.
// Locked artists are skipped because Run Rules already skips them; bumping
// their dirty_since would just create permanently-dirty rows that never get
// cleared. Returns the number of rows affected so callers can log the impact.
func (r *sqliteArtistRepo) MarkAllDirty(ctx context.Context, ts time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE artists SET dirty_since = ? WHERE is_excluded = 0 AND locked = 0`,
		ts.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("marking all artists dirty: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading rows affected for mark-all-dirty: %w", err)
	}
	return n, nil
}

// MarkRulesEvaluated stamps rules_evaluated_at for a single artist after the
// rule pipeline finishes processing it. The timestamp is set to ts (UTC).
// Combined with MarkDirty this is what lets the pipeline filter out artists
// that have not changed since the last run.
func (r *sqliteArtistRepo) MarkRulesEvaluated(ctx context.Context, id string, ts time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE artists SET rules_evaluated_at = ? WHERE id = ?`,
		ts.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("marking artist rules-evaluated: %w", err)
	}
	return nil
}

// ListDirtyIDs returns the IDs of non-excluded, non-locked artists that need
// rule re-evaluation on the next incremental pass. An artist is dirty when:
//
//  1. It has never been evaluated (rules_evaluated_at IS NULL), OR
//  2. It has mutated since the last evaluation (dirty_since > rules_evaluated_at), OR
//  3. Any currently-enabled rule was updated since the last evaluation.
//
// Condition 3 is the self-healing branch: a rule mutation bumps rules.updated_at,
// and this query picks up every artist whose rules_evaluated_at predates that
// bump. This replaces what used to be a broad "mark all artists dirty"
// side-effect of rule create/update. Benefits over the side-effect approach:
//
//   - No failure mode where the rule write commits but the dirty-mark sweep
//     fails and the new rule silently never fires.
//   - Disabling a rule does not trigger a full-library re-evaluation (the
//     JOIN filters on enabled = 1, so disabled rule updates do not surface).
//   - No extra write amplification on rule edits.
//
// Timestamp columns are normalized via datetime() before comparison. The
// schema defaults rules.updated_at to datetime('now') (space-separated), while
// artists.rules_evaluated_at and dirty_since are stamped with RFC3339 (T-separated).
// A raw TEXT comparison would sort "2026-04-18 12:00:00" below "2026-04-18T00:00:00Z"
// and same-day rule edits on seeded rows would fail to re-dirty already-evaluated
// artists. datetime() canonicalizes both sides for a chronological comparison.
//
// Sorted by name for stable progress reporting. Locked artists are excluded
// because the pipeline skips them; including them would inflate progress counters.
func (r *sqliteArtistRepo) ListDirtyIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM artists
		WHERE is_excluded = 0
		  AND locked = 0
		  AND (
		    rules_evaluated_at IS NULL
		    OR (dirty_since IS NOT NULL AND datetime(dirty_since) > datetime(rules_evaluated_at))
		    OR EXISTS (
		      SELECT 1 FROM rules
		      WHERE enabled = 1
		        AND datetime(updated_at) > datetime(artists.rules_evaluated_at)
		    )
		  )
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("querying dirty artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning dirty artist id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dirty artists: %w", err)
	}
	return ids, nil
}

// CountEligibleArtists returns the number of non-excluded, non-locked
// artists in the catalog. This is the denominator for the "evaluating N of
// M (M-N unchanged)" progress reporting in Run Rules.
func (r *sqliteArtistRepo) CountEligibleArtists(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE is_excluded = 0 AND locked = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting eligible artists: %w", err)
	}
	return n, nil
}
