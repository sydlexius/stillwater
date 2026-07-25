package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteHistoryRepo struct {
	db *sql.DB
}

func newSQLiteHistoryRepo(db *sql.DB) HistoryRepository {
	return &sqliteHistoryRepo{db: db}
}

// Record inserts a new metadata change row.
func (r *sqliteHistoryRepo) Record(ctx context.Context, change *MetadataChange) error {
	const q = `
		INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := r.db.ExecContext(ctx, q,
		change.ID,
		change.ArtistID,
		change.Field,
		change.OldValue,
		change.NewValue,
		change.Source,
		change.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("inserting metadata change: %w", err)
	}
	return nil
}

// GetByID retrieves a single metadata change by its primary key.
func (r *sqliteHistoryRepo) GetByID(ctx context.Context, id string) (*MetadataChange, error) {
	const q = `
		SELECT id, artist_id, field, old_value, new_value, source, created_at
		FROM metadata_changes
		WHERE id = ?`

	var c MetadataChange
	var createdAtStr string
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&c.ID, &c.ArtistID, &c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrChangeNotFound, id)
		}
		return nil, fmt.Errorf("querying metadata change: %w", err)
	}

	c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
	return &c, nil
}

// List returns paginated metadata changes for an artist, ordered by created_at DESC.
// Returns the changes for the requested page and the total count across all pages.
func (r *sqliteHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error) {
	// Fetch total count first.
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = ?`, artistID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting metadata changes: %w", err)
	}

	if total == 0 {
		return []MetadataChange{}, 0, nil
	}

	// Migration 004 normalized any legacy "YYYY-MM-DD HH:MM:SS" rows to
	// RFC3339, so a plain TEXT compare is monotonic again. Direct ORDER
	// BY on created_at is index-friendly without a datetime() wrapper.
	const q = `
		SELECT id, artist_id, field, old_value, new_value, source, created_at
		FROM metadata_changes
		WHERE artist_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, q, artistID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying metadata changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var changes []MetadataChange
	for rows.Next() {
		var c MetadataChange
		var createdAtStr string
		if err := rows.Scan(&c.ID, &c.ArtistID, &c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr); err != nil {
			return nil, 0, fmt.Errorf("scanning metadata change row: %w", err)
		}
		c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating metadata change rows: %w", err)
	}

	return changes, total, nil
}

// ListGlobal returns paginated metadata changes across all artists, joining
// the artists table to include the artist name in each result.
// When filter.PerFieldLimit > 0 a windowed CTE query is used that returns
// at most PerFieldLimit rows per distinct field (ROW_NUMBER() OVER
// PARTITION BY mc.field); the global Limit/Offset fields are ignored on
// that path and the returned total is always 0.
func (r *sqliteHistoryRepo) ListGlobal(ctx context.Context, filter GlobalHistoryFilter) ([]MetadataChangeWithArtist, int, error) {
	if filter.PerFieldLimit > 0 {
		return r.listGlobalPerFieldCapped(ctx, filter)
	}

	// Build dynamic WHERE clause.
	var where []string
	var args []any

	if filter.ArtistID != "" {
		where = append(where, "mc.artist_id = ?")
		args = append(args, filter.ArtistID)
	}
	if len(filter.Fields) > 0 {
		placeholders := make([]string, len(filter.Fields))
		for i, f := range filter.Fields {
			placeholders[i] = "?"
			args = append(args, f)
		}
		where = append(where, "mc.field IN ("+strings.Join(placeholders, ", ")+")")
	}
	// Build source filter combining exact matches and prefix matches (e.g. "provider:*").
	if len(filter.Sources) > 0 || len(filter.SourcePrefixes) > 0 {
		var sourceClauses []string
		if len(filter.Sources) > 0 {
			placeholders := make([]string, len(filter.Sources))
			for i, s := range filter.Sources {
				placeholders[i] = "?"
				args = append(args, s)
			}
			sourceClauses = append(sourceClauses, "mc.source IN ("+strings.Join(placeholders, ", ")+")")
		}
		for _, prefix := range filter.SourcePrefixes {
			sourceClauses = append(sourceClauses, "mc.source LIKE ? ESCAPE '\\'")
			escaped := dbutil.EscapeLike(prefix)
			args = append(args, escaped+"%")
		}
		where = append(where, "("+strings.Join(sourceClauses, " OR ")+")")
	}

	// Migration 004 normalized legacy space-separator rows to RFC3339, so
	// direct text compares are monotonic. Bind RFC3339 directly to keep
	// the index on created_at usable.
	if !filter.From.IsZero() {
		where = append(where, "mc.created_at >= ?")
		args = append(args, filter.From.UTC().Format(time.RFC3339))
	}
	if !filter.To.IsZero() {
		where = append(where, "mc.created_at <= ?")
		args = append(args, filter.To.UTC().Format(time.RFC3339))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Count total matching rows. The JOIN ensures orphaned metadata_changes
	// rows (where the artist was deleted) are excluded from the count,
	// matching the behavior of the select query below.
	countQ := "SELECT COUNT(*) FROM metadata_changes mc JOIN artists a ON a.id = mc.artist_id " + whereClause
	var total int
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting global metadata changes: %w", err)
	}
	if total == 0 {
		return []MetadataChangeWithArtist{}, 0, nil
	}

	// Plain TEXT ORDER BY on created_at is monotonic post-migration 004.
	selectQ := `
		SELECT mc.id, mc.artist_id, a.name, mc.field, mc.old_value, mc.new_value, mc.source, mc.created_at
		FROM metadata_changes mc
		JOIN artists a ON a.id = mc.artist_id
		` + whereClause + `
		ORDER BY mc.created_at DESC, mc.id DESC
		LIMIT ? OFFSET ?`

	queryArgs := make([]any, 0, len(args)+2)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, selectQ, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying global metadata changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var changes []MetadataChangeWithArtist
	for rows.Next() {
		var c MetadataChangeWithArtist
		var createdAtStr string
		if err := rows.Scan(
			&c.ID, &c.ArtistID, &c.ArtistName,
			&c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning global metadata change row: %w", err)
		}
		c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating global metadata change rows: %w", err)
	}

	return changes, total, nil
}

// listGlobalPerFieldCapped returns the most-recent filter.PerFieldLimit rows
// per distinct field using a windowed CTE query. This is the path taken when
// GlobalHistoryFilter.PerFieldLimit > 0. The returned total is always 0
// (windowed results have no meaningful pagination total).
//
// SQLite supports window functions since 3.25.0 (2018-09-15). The CTE uses
// ROW_NUMBER() OVER (PARTITION BY mc.field ORDER BY created_at DESC, id DESC)
// so each field gets its own independent top-N cut.
func (r *sqliteHistoryRepo) listGlobalPerFieldCapped(ctx context.Context, filter GlobalHistoryFilter) ([]MetadataChangeWithArtist, int, error) {
	// Build the same WHERE clause as ListGlobal (artist_id, fields, etc.)
	var where []string
	var args []any

	if filter.ArtistID != "" {
		where = append(where, "mc.artist_id = ?")
		args = append(args, filter.ArtistID)
	}
	if len(filter.Fields) > 0 {
		placeholders := make([]string, len(filter.Fields))
		for i, f := range filter.Fields {
			placeholders[i] = "?"
			args = append(args, f)
		}
		where = append(where, "mc.field IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(filter.Sources) > 0 || len(filter.SourcePrefixes) > 0 {
		var sourceClauses []string
		if len(filter.Sources) > 0 {
			placeholders := make([]string, len(filter.Sources))
			for i, s := range filter.Sources {
				placeholders[i] = "?"
				args = append(args, s)
			}
			sourceClauses = append(sourceClauses, "mc.source IN ("+strings.Join(placeholders, ", ")+")")
		}
		for _, prefix := range filter.SourcePrefixes {
			sourceClauses = append(sourceClauses, "mc.source LIKE ? ESCAPE '\\'")
			escaped := dbutil.EscapeLike(prefix)
			args = append(args, escaped+"%")
		}
		where = append(where, "("+strings.Join(sourceClauses, " OR ")+")")
	}
	if !filter.From.IsZero() {
		where = append(where, "mc.created_at >= ?")
		args = append(args, filter.From.UTC().Format(time.RFC3339))
	}
	if !filter.To.IsZero() {
		where = append(where, "mc.created_at <= ?")
		args = append(args, filter.To.UTC().Format(time.RFC3339))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// CTE: rank each row within its field partition, most-recent first.
	// The outer query filters to rn <= PerFieldLimit and orders by field then rank
	// so results are grouped by field in a predictable order.
	selectQ := `
		WITH ranked AS (
			SELECT
				mc.id, mc.artist_id, a.name, mc.field,
				mc.old_value, mc.new_value, mc.source, mc.created_at,
				ROW_NUMBER() OVER (
					PARTITION BY mc.field
					ORDER BY mc.created_at DESC, mc.id DESC
				) AS rn
			FROM metadata_changes mc
			JOIN artists a ON a.id = mc.artist_id
			` + whereClause + `
		)
		SELECT id, artist_id, name, field, old_value, new_value, source, created_at
		FROM ranked
		WHERE rn <= ?
		ORDER BY field, rn`

	queryArgs := make([]any, 0, len(args)+1)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, filter.PerFieldLimit)

	rows, err := r.db.QueryContext(ctx, selectQ, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying per-field-capped metadata changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	changes := make([]MetadataChangeWithArtist, 0)
	for rows.Next() {
		var c MetadataChangeWithArtist
		var createdAtStr string
		if err := rows.Scan(
			&c.ID, &c.ArtistID, &c.ArtistName,
			&c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning per-field-capped metadata change row: %w", err)
		}
		c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating per-field-capped metadata change rows: %w", err)
	}

	return changes, 0, nil
}

// blastAutomatedExactSources and blastAutomatedSourcePrefixes are the SINGLE
// definition of "an automated writer made this change". Both the SQL predicates
// and the Go classifier are derived from these two slices, so there is no second
// copy that can drift.
//
// That drift is not hypothetical: the SQL and the Go classifier previously
// carried separate hand-written copies, and their "unknown" halves disagreed.
// SQL called a row unknown only when source was literally "manual", while Go
// called anything not recognized as automated unknown. A source that is neither
// (this codebase writes "revert", and a future one could write anything) was
// therefore listed in the rows but counted in neither bucket, so the operator
// saw a total that silently omitted rows. See blastAttributionPredicate for how
// the complement now makes that impossible.
var (
	// blastAutomatedExactSources match the source column exactly.
	blastAutomatedExactSources = []string{"scan", "import"}
	// blastAutomatedSourcePrefixes match the start of the source column. The
	// values after the colon vary at runtime (a provider name, a rule id), so
	// these are prefix matches rather than exact ones.
	blastAutomatedSourcePrefixes = []string{"provider:", "rule:"}
)

// blastRadiusAutomatedSQL builds the "automated writer" predicate over the given
// source column expression. col varies because the CTE's columns are bare
// ("source") inside the outer select but qualified ("mc.source") inside the CTE.
//
// The values are interpolated rather than bound as query parameters because they
// are compile-time constants above, containing no quote characters and no LIKE
// wildcards ("%" or "_"). Nothing caller-supplied reaches this string. The
// prefixes are likewise literal LIKE patterns rather than dbutil.EscapeLike
// calls: there is nothing to escape.
func blastRadiusAutomatedSQL(col string) string {
	quoted := make([]string, 0, len(blastAutomatedExactSources))
	for _, s := range blastAutomatedExactSources {
		quoted = append(quoted, "'"+s+"'")
	}
	parts := make([]string, 0, 1+len(blastAutomatedSourcePrefixes))
	parts = append(parts, col+" IN ("+strings.Join(quoted, ", ")+")")
	for _, p := range blastAutomatedSourcePrefixes {
		parts = append(parts, col+" LIKE '"+p+"%'")
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// blastAttributionPredicate returns the SQL predicate selecting ONE attribution
// bucket over the given source column, or "" for "both buckets".
//
// The unknown predicate is the exact COMPLEMENT of the automated one, mirroring
// classifyBlastAttribution's default branch. This is the property that keeps the
// counts and the rows honest: every row that reaches this point falls in exactly
// one bucket, so no row can appear in the list and be counted in neither. It
// also means the two buckets are exhaustive, which is why the "both" case adds
// no clause at all -- "automated OR NOT automated" is true for every row
// (source is NOT NULL, so there is no third truth value to worry about).
//
// See BlastAttributionUnknown for why "cannot prove automated" is reported as
// unknown rather than assumed to be a clean operator edit.
func blastAttributionPredicate(attribution, col string) string {
	switch attribution {
	case BlastAttributionAutomated:
		return blastRadiusAutomatedSQL(col)
	case BlastAttributionUnknown:
		return "NOT " + blastRadiusAutomatedSQL(col)
	default:
		return ""
	}
}

// blastRadiusRankedCTE ranks every metadata_changes row within its
// (artist_id, field) partition, most recent first.
//
// # WHY THE DAMAGE PREDICATES ARE NOT IN HERE
//
// This CTE deliberately carries NO filter on old_value, new_value, or source.
// Every one of those belongs in the OUTER select, and moving any of them inward
// is the single most dangerous "optimization" available in this file.
//
// The frame answers "what is the LATEST thing that happened to this field",
// and only then asks "is that latest thing damage". Filtering inward inverts
// it into "what is the latest DAMAGE to this field", which is a different and
// wrong question: a field that was destroyed and later put back would have its
// old damage row promoted to rank 1 and would be reported as broken forever,
// no matter how many times an operator recovered it.
//
// Concretely, a recovery writes a NEWER row for the same (artist_id, field)
// whose source is "revert". Because that row wins the ranking here, the outer
// select sees it, recognizes it as a recovery, and drops the pair from the
// report. That is the entire mechanism by which restored fields disappear, and
// it only works while this CTE stays unfiltered.
const blastRadiusRankedCTE = `
	WITH ranked AS (
		SELECT
			mc.id, mc.artist_id, a.name AS artist_name, mc.field,
			mc.old_value, mc.new_value, mc.source, mc.created_at,
			ROW_NUMBER() OVER (
				PARTITION BY mc.artist_id, mc.field
				ORDER BY mc.created_at DESC, mc.id DESC
			) AS rn
		FROM metadata_changes mc
		JOIN artists a ON a.id = mc.artist_id
		%s
	)`

// blastRadiusRankedWhere builds the WHERE clause applied INSIDE the ranking
// CTE. Only narrowing that cannot change which row is newest per
// (artist_id, field) is allowed here -- restricting to one artist or one field
// removes whole partitions rather than reordering them within one, so both are
// safe. No damage or source predicate may join them; see blastRadiusRankedCTE.
func blastRadiusRankedWhere(f BlastRadiusFilter) (string, []any) {
	var where []string
	var args []any
	if f.ArtistID != "" {
		where = append(where, "mc.artist_id = ?")
		args = append(args, f.ArtistID)
	}
	if f.Field != "" {
		where = append(where, "mc.field = ?")
		args = append(args, f.Field)
	}
	if len(where) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

// blastRadiusDamageWhere builds the OUTER select's WHERE clause: take only the
// newest row per (artist_id, field), keep it only if it represents an
// operator's value being destroyed, and classify who did it.
//
// attribution selects which buckets to include and is the ONE part callers vary
// between the row query (which honors the operator's filter) and the count
// query (which deliberately ignores it so both buckets are always counted).
func blastRadiusDamageWhere(class, attribution string) string {
	where := []string{
		"rn = 1",
		// The operator had a value. Without this a first-ever population of an
		// empty field would read as damage.
		"old_value != ''",
		// Something replaced it. A no-op rewrite is not damage.
		"old_value != new_value",
		// A recovery is not damage. Recovering a "replaced" field writes a
		// revert row whose old_value is the wrong value and whose new_value is
		// the recovered one, so it satisfies both predicates above and would
		// otherwise read as fresh damage.
		//
		// This is now the ONE AND ONLY place "revert" is excluded, and that is
		// deliberate. The attribution buckets below are exhaustive (automated
		// and its complement), so they no longer drop anything on their own: an
		// unrecognized source lands in the unknown bucket and IS reported,
		// which is the point. Excluding a source from the report is therefore a
		// decision that has to be written down here, on purpose, rather than
		// something that falls out of two predicates happening to disagree.
		"source != 'revert'",
	}

	switch class {
	case BlastClassBlanked:
		where = append(where, "new_value = ''")
	case BlastClassReplaced:
		where = append(where, "new_value != ''")
	}

	// Note the bare column name: this clause runs in the OUTER select, where the
	// CTE has already projected "source" without the "mc." table qualifier.
	//
	// An empty predicate means "both buckets", which needs no clause because the
	// two buckets are exhaustive by construction. See blastAttributionPredicate.
	if p := blastAttributionPredicate(attribution, "source"); p != "" {
		where = append(where, p)
	}

	return "WHERE " + strings.Join(where, " AND ")
}

// blastRadiusOrderBy maps a validated sort key to a SQL ORDER BY clause. The
// key and direction come from BlastRadiusFilter.Validate, which coerces
// anything unrecognized to a default, so no caller-supplied text reaches SQL.
func blastRadiusOrderBy(sortKey, order string) string {
	dir := "DESC"
	if order == "asc" {
		dir = "ASC"
	}
	switch sortKey {
	case BlastSortArtistName:
		return "ORDER BY artist_name " + dir + ", field ASC, id DESC"
	case BlastSortField:
		return "ORDER BY field " + dir + ", artist_name ASC, id DESC"
	default:
		// created_at is stored RFC3339 (migration 004 normalized the legacy
		// space-separated rows), so a plain TEXT sort is chronological.
		return "ORDER BY created_at " + dir + ", id DESC"
	}
}

// ListBlastRadius returns the currently-destroyed fields across the library:
// for each (artist, field), the most recent change, kept only when that change
// was an automated writer replacing a value the operator had.
//
// Already-recovered fields are absent by construction -- see
// blastRadiusRankedCTE. This is therefore a CURRENT-STATE view, not a
// historical log: a field damaged twice appears once, showing the latest
// damage. The per-artist history tab remains the full log.
func (r *sqliteHistoryRepo) ListBlastRadius(ctx context.Context, f BlastRadiusFilter) ([]BlastRadiusRow, error) {
	f.Validate()

	cteWhere, args := blastRadiusRankedWhere(f)
	//nolint:gosec // G202: every concatenated fragment is server-built. cteWhere
	// emits only "?" placeholders (its values are bound as args); the damage and
	// order clauses are selected by switch from the validated Class/Attribution/
	// Sort/Order constants. No caller-supplied text reaches the string.
	q := fmt.Sprintf(blastRadiusRankedCTE, cteWhere) + `
		SELECT id, artist_id, artist_name, field, old_value, new_value, source, created_at
		FROM ranked
		` + blastRadiusDamageWhere(f.Class, f.Attribution) + `
		` + blastRadiusOrderBy(f.Sort, f.Order) + `
		LIMIT ? OFFSET ?`

	queryArgs := make([]any, 0, len(args)+2)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying blast-radius rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]BlastRadiusRow, 0)
	for rows.Next() {
		var row BlastRadiusRow
		var createdAtStr string
		if err := rows.Scan(
			&row.ID, &row.ArtistID, &row.ArtistName,
			&row.Field, &row.OldValue, &row.NewValue, &row.Source, &createdAtStr,
		); err != nil {
			return nil, fmt.Errorf("scanning blast-radius row: %w", err)
		}
		row.CreatedAt = parseHistoryTimestamp(row.ID, createdAtStr)
		row.Class = classifyBlastDamage(row.NewValue)
		row.Attribution = classifyBlastAttribution(row.Source)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating blast-radius rows: %w", err)
	}
	return out, nil
}

// CountBlastRadius returns how the matching rows split by attribution.
//
// The two bucket counts deliberately IGNORE f.Attribution. An operator who has
// filtered the table down to one bucket must still see how many rows are in the
// other, because a view that can silently drop the unattributable rows is the
// "unknown rendered as clean" defect this report exists to avoid.
func (r *sqliteHistoryRepo) CountBlastRadius(ctx context.Context, f BlastRadiusFilter) (BlastRadiusCounts, error) {
	f.Validate()
	cteWhere, args := blastRadiusRankedWhere(f)

	// ONE scan over the ranked rows, splitting the buckets with conditional
	// aggregation rather than running the same query twice with different
	// attribution clauses. Two scans could return figures that disagree with
	// each other (and historically the two clauses did); a single scan cannot
	// disagree with itself.
	//
	// The damage clause is built with BlastScopeAll on purpose: it selects every
	// damaged row regardless of the operator's attribution filter, which is what
	// the "both buckets are always reported" contract requires.
	//
	// Same server-built fragments as ListBlastRadius: cteWhere emits only "?"
	// placeholders and the damage clause is switch-selected from validated
	// constants, so no caller-supplied text reaches the string.
	automated := blastRadiusAutomatedSQL("source")
	q := fmt.Sprintf(blastRadiusRankedCTE, cteWhere) + `
		SELECT
			SUM(CASE WHEN ` + automated + ` THEN 1 ELSE 0 END),
			SUM(CASE WHEN ` + automated + ` THEN 0 ELSE 1 END)
		FROM ranked ` + blastRadiusDamageWhere(f.Class, BlastScopeAll)

	// SUM over zero rows is NULL in SQLite, not 0, so both columns are scanned
	// as nullable and a NULL is read as an honest zero.
	var automatedN, unknownN sql.NullInt64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&automatedN, &unknownN); err != nil {
		return BlastRadiusCounts{}, fmt.Errorf("counting blast-radius rows by attribution: %w", err)
	}

	var counts BlastRadiusCounts
	counts.Automated = int(automatedN.Int64)
	counts.Unknown = int(unknownN.Int64)

	// Total follows the ACTIVE filter because pagination needs it; the bucket
	// counts above do not, because honesty needs them not to.
	switch f.Attribution {
	case BlastAttributionAutomated:
		counts.Total = counts.Automated
	case BlastAttributionUnknown:
		counts.Total = counts.Unknown
	default:
		counts.Total = counts.Automated + counts.Unknown
	}
	return counts, nil
}

// classifyBlastDamage labels a row by what happened to the operator's value.
//
// Only newValue is needed: callers reach this exclusively for rows the query
// has already confirmed are damage, so "the operator had a value"
// (old_value != ”) and "something replaced it" (old_value != new_value) both
// hold by construction. What remains is whether the replacement was empty.
func classifyBlastDamage(newValue string) string {
	if newValue == "" {
		return BlastClassBlanked
	}
	return BlastClassReplaced
}

// classifyBlastAttribution labels a row by who Stillwater can prove made the
// change. Anything not positively recognized as an automated writer is
// unknown, never assumed clean.
//
// This reads the SAME two collections the SQL predicates are built from, so the
// classifier that labels a row and the predicate that counts it cannot disagree
// about what "automated" means.
func classifyBlastAttribution(source string) string {
	for _, s := range blastAutomatedExactSources {
		if source == s {
			return BlastAttributionAutomated
		}
	}
	for _, p := range blastAutomatedSourcePrefixes {
		if strings.HasPrefix(source, p) {
			return BlastAttributionAutomated
		}
	}
	return BlastAttributionUnknown
}

// parseHistoryTimestamp parses a created_at string from the metadata_changes
// table, trying RFC3339 first, then SQLite datetime format. Falls back to
// current time with a warning if both fail.
func parseHistoryTimestamp(changeID, raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t, err = time.Parse(time.DateTime, raw)
		if err != nil {
			slog.Warn("unparsable created_at in metadata_changes",
				"change_id", changeID,
				"raw_value", raw,
				"error", err,
			)
			return time.Now().UTC()
		}
	}
	return t.UTC()
}
