package artist

// name_collision.go -- pre-write guard for a rename that would collapse two
// artists onto one identity (#2730).
//
// The defect this closes: renaming a platform-only artist (an Emby/Jellyfin
// record with no filesystem path) to a name an existing artist already holds
// silently produced two same-named entries. Nothing rejected the write, so the
// operator only discovered the duplicate afterwards, in the duplicates report.
//
// The check runs BEFORE the write and uses the SAME identity key that
// duplicate detection uses (NormalizeIdentityKey). Using the same key is the
// load-bearing property: a guard keyed on raw string equality would let
// "Nirvana" and "nirvana " through, and DetectDuplicates would then group the
// two rows the guard just allowed. Keying both on NormalizeIdentityKey means
// "the guard rejects exactly what the report would have flagged".
//
// WHERE ENFORCEMENT LIVES (rewritten in #3037; the previous text described the
// opposite arrangement and is gone rather than left to contradict the code).
// The guard is a SERVICE-level gate: Service.UpdateField routes a "name" write
// to UpdateNameGuarded, so a caller cannot reach the name column by picking a
// different SINGLE-FIELD service method. The API boundary keeps its own
// pre-write check (guardNameCollision) purely as a fast path that produces a
// nicer failure before any transaction opens; it is no longer the guarantee.
//
// The earlier reasoning -- that history-revert and platform-state sync "must
// stay able to restore a prior value", so the service must not gate -- was
// wrong in the one case that mattered. Undoing a rename that DE-DUPLICATED two
// artists recreates the duplicate, which is the exact state #2730 exists to
// prevent; "it was the prior value" does not make it safe to write now. Those
// flows are not broken by the gate, they are CORRECTED by it: a refused revert
// reports a collision instead of silently recreating one. (History revert is
// LIVE on this base: #3037 added "name" and "sort_name" to trackableFields, so
// an Undo of a rename now reaches UpdateField and takes this gate -- see
// updateNameThroughGuard. Platform-state sync still cannot drive a name write
// at all: it calls UpdateField only for biography, genres and the
// formed/born and disbanded/died dates.)
//
// SCOPE, STATED HONESTLY. Every statement in this file's SQL that can write the
// artists.name column was enumerated with
// `grep -rn 'UPDATE artists\|INSERT INTO artists' internal/artist --include='*.go' | grep -v _test`
// and reading each hit for a name assignment -- the sentence is about
// PRODUCTION statements because that grep filters tests out. Four can:
// UpdateNameGuarded's own targeted UPDATE, the repository's two dynamic
// column-map UPDATE statements (which resolve "name" through fieldColumnMap),
// the whole-row UPDATE in sqlite_artist.go, and the row INSERT. This routing
// covers the first three -- direct UpdateNameGuarded callers plus
// Service.UpdateField and Service.ClearField, the only service methods that
// reach those two. TWO service methods reach the name column
// WITHOUT it:
//
//   - Service.Update, the whole-row persist (the multi-column UPDATE in
//     sqlite_artist.go). A caller that mutates a.Name and persists the struct
//     writes the name column with no collision check. Reachable from
//     applyProviderName (internal/api/handlers_refresh.go), the
//     name_language_pref fixer (internal/rule/fixers_language.go) and
//     internal/artist/disambiguation.go.
//   - Service.Create, the row INSERT. It reaches the raw INSERT INTO artists
//     with neither validation nor a collision check, so an insert can seed an
//     artist whose name is empty or one a different artist already holds --
//     the guard only ever sees an UPDATE. Reachable in production from the
//     Emby/Jellyfin library populate handlers
//     (internal/api/handlers_connection_library.go) and the filesystem scanner
//     (internal/scanner/scanner.go).
//
// Both gaps predate #3037 and are deliberately NOT closed here; closing them
// means deciding what a scanner, an importer and a merge engine should do when
// the incoming row is invalid or collides, which is its own change.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrNameCollision reports that a requested name change would give the artist
// the same identity as a DIFFERENT existing artist. Callers translate this to
// HTTP 409 and steer the operator at the merge/reconcile flow instead of
// writing a second same-named record.
var ErrNameCollision = errors.New("another artist already uses this name")

// NameCollision describes the existing artist a rename would collide with.
// It is returned alongside a nil error by FindNameCollision so the caller can
// build a message naming the other record; the sentinel above is what callers
// match on when a collision is turned into a returned error.
type NameCollision struct {
	// ArtistID is the existing artist that already holds the identity.
	ArtistID string
	// Name is that artist's stored name, as displayed to the operator. It is
	// the raw name, NOT the normalized key: the key is an internal matching
	// artifact and would be confusing in user-facing copy.
	Name string
	// Path is that artist's filesystem path, empty for a platform-only record.
	Path string
}

// PlatformOnly reports whether the colliding artist exists only as a platform
// (Emby / Jellyfin / Lidarr) record, with no directory on disk. Mirrors the
// PlatformOnly flag on NearDuplicateArtist so both surfaces answer the
// question the same way.
func (c *NameCollision) PlatformOnly() bool {
	return c != nil && strings.TrimSpace(c.Path) == ""
}

// NameCollisionError is the ERROR form of a refused rename, for the call
// shapes that have no second return value to carry a *NameCollision.
//
// Service.UpdateField is the reason it exists. That method returns
// (changed bool, err error), and a rename it refuses is neither a successful
// write nor a no-op: reporting it as (false, nil) would tell the caller
// "nothing needed doing", which is the exact lie a refusal must never become.
// So the refusal travels as an error that still carries the colliding artist,
// letting a caller render the SAME refusal the direct UpdateNameGuarded call
// site renders.
//
// errors.Is(err, ErrNameCollision) matches it, so a caller that only needs to
// classify the failure never has to type-assert.
type NameCollisionError struct {
	// Collision names the artist that already holds the identity. Never nil
	// for an error this package constructs.
	Collision *NameCollision
}

// Error renders the sentinel's text plus the colliding artist's name, so a
// log line that only prints the error is still actionable.
func (e *NameCollisionError) Error() string {
	if e == nil || e.Collision == nil {
		return ErrNameCollision.Error()
	}
	return fmt.Sprintf("%s: %q (artist %s)", ErrNameCollision.Error(), e.Collision.Name, e.Collision.ArtistID)
}

// Unwrap makes errors.Is(err, ErrNameCollision) true, so callers classify on
// the sentinel and only type-assert when they need the colliding artist.
func (e *NameCollisionError) Unwrap() error { return ErrNameCollision }

// FindNameCollision reports whether renaming artistID to newName would land it
// on an identity key that a different artist already holds.
//
// This is a FAST PATH, not the authority. It answers from a snapshot taken
// before the write, so a concurrent rename toward the same target can land
// between this call and the update. UpdateNameGuarded re-runs the same check
// inside the writing transaction and is what actually decides; this call
// exists so the ordinary (uncontended) case reports the collision without
// opening a transaction at all.
//
// Returns (nil, nil) when the rename is safe. A non-nil *NameCollision means
// the write must not proceed. A non-nil error means the check could not be
// completed -- callers MUST treat that as a failure and refuse the write
// rather than falling through to the unguarded path, since "the guard could
// not run" is not evidence that the rename is safe.
//
// Two cases are deliberately NOT collisions:
//
//   - An empty normalized key. ValidateFieldUpdate refuses a "name" whose key
//     normalizes to "" (blank, or made up entirely of dashes, underscores,
//     spacing, or invisible formatting characters), and as of #3037 every
//     single-field service write verb runs it first, so this exemption is
//     unreachable from those paths and cannot become the hole it once was. The
//     exemption stays because FindNameCollision is also callable directly as a
//     pre-write probe, where reporting a "collision" for a name that has no
//     identity at all would produce a misleading message. It is NOT covered on
//     the two methods named in the scope block above, Service.Update (the
//     whole-row persist) and Service.Create (the row INSERT).
//   - A new name whose key equals the artist's CURRENT key. Editing "The Cure"
//     to "Cure" is a cosmetic change that does not create a second identity,
//     so it stays allowed. Without this case the guard would block an operator
//     from tidying an artist's own name.
func (s *Service) FindNameCollision(ctx context.Context, artistID, newName string) (*NameCollision, error) {
	newKey := NormalizeIdentityKey(newName)
	if newKey == "" {
		return nil, nil
	}

	// The artist's own current name. A rename that keeps the same identity key
	// cannot create a duplicate, so it is allowed through.
	current, err := s.artists.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("checking name collision: loading artist %s: %w", artistID, err)
	}
	if NormalizeIdentityKey(current.Name) == newKey {
		return nil, nil
	}

	db, err := s.artistDB()
	if err != nil {
		return nil, fmt.Errorf("checking name collision: %w", err)
	}
	if db == nil {
		return nil, fmt.Errorf("checking name collision: nil database handle")
	}

	// The scan itself is shared with the in-transaction check so both paths
	// decide "is this a duplicate?" with one implementation.
	return findCollisionPartner(ctx, db, artistID, newKey)
}

// rowQuerier is the read surface findCollisionPartner needs. Both *sql.DB and
// *sql.Tx satisfy it, which is the point: the pre-write guard scans on the
// pool while UpdateNameGuarded scans INSIDE its own write transaction, and
// both must reach the identical comparison. Two copies of the scan would be
// free to drift, and a drift here means "the fast path and the authoritative
// path disagree about what a duplicate is" -- the exact failure this guard
// exists to prevent.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// findCollisionPartner scans every artist OTHER than artistID and returns the
// first whose normalized identity key equals newKey.
//
// The key is computed by a Go function (Unicode normalization, punctuation
// folding, article stripping), so it cannot be expressed as a SQL predicate
// and there is no stored key column to index. This is the same whole-table
// approach DetectDuplicates takes, at the same scale (one row per artist), and
// it runs only on a name edit -- a rare, human-driven action.
//
// ORDER BY makes the reported partner deterministic when more than one
// existing artist shares the key, so the operator sees a stable message
// instead of one that changes between identical attempts. Pinned by
// TestFindNameCollision_MultiplePartnersIsDeterministic, which seeds two
// artists sharing the target key with IDs chosen so a different ordering
// would select the other row.
//
// newKey MUST be a non-empty key already produced by NormalizeIdentityKey;
// callers handle the empty-key case before calling.
func findCollisionPartner(ctx context.Context, q rowQuerier, artistID, newKey string) (*NameCollision, error) {
	const query = `SELECT id, name, path FROM artists WHERE id <> ? ORDER BY name, id`
	rows, err := q.QueryContext(ctx, query, artistID)
	if err != nil {
		return nil, fmt.Errorf("checking name collision: querying artists: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, name, path string
		if err := rows.Scan(&id, &name, &path); err != nil {
			return nil, fmt.Errorf("checking name collision: scanning artist row: %w", err)
		}
		if NormalizeIdentityKey(name) == newKey {
			return &NameCollision{ArtistID: id, Name: name, Path: path}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking name collision: iterating artists: %w", err)
	}

	return nil, nil
}

// UpdateNameGuarded writes artistID's name field only if no OTHER artist holds
// the resulting identity key, deciding both inside ONE transaction.
//
// This is the authoritative half of the #2730 guard. FindNameCollision run
// before the write is a fast path: it answers from a snapshot that is already
// stale by the time the write lands, so two concurrent renames toward the same
// target name could both pass it and both write -- producing exactly the
// duplicate the guard exists to prevent (TOCTOU: time-of-check to time-of-use,
// the gap between validating a condition and acting on it).
//
// WHAT MAKES THIS SAFE, precisely, because the obvious answer is wrong:
//
// The check and the write are in ONE transaction, and transactions here cannot
// interleave -- the connection pool is capped at a SINGLE connection
// (database.go, `db.SetMaxOpenConns(1)`). database/sql therefore blocks a
// second BeginTx until the first transaction returns its connection, so the
// second rename's check cannot run until the first has committed and is
// visible to it. That serialization is the whole mechanism.
//
// The step order below is:
//
//  1. read the artist's CURRENT name (needed for the self-rename exemption
//     and for the history record, and must be the pre-write value)
//  2. write the new name
//  3. re-scan for a colliding partner
//  4. commit, or roll the write back and report the collision
//
// THE UPDATE-BEFORE-CHECK ORDER IS NOT LOAD-BEARING at the current pool
// setting. This was measured, not assumed: moving the check ahead of the write
// leaves TestUpdateNameGuarded_ConcurrentRenamesToSameTarget passing, because
// the two renames never reach SQLite at the same time and SQLite's locking is
// never consulted. Do not reorder these statements believing it changes the
// concurrency behavior -- it does not. (Equally, do not "restore" a rationale
// about deferred transactions and write locks: which lock the transaction
// takes is irrelevant while only one transaction can be open at a time.)
//
// IF THE POOL CAP IS EVER RAISED, this degrades in a specific way, also
// measured: with several connections available the two transactions do reach
// SQLite together, and the losing writer fails with SQLITE_BUSY ("database is
// locked") instead of writing. The invariant still holds -- at most one artist
// ends up with the identity -- so it fails CLOSED, but the refused operator
// sees a 500 rather than the clean 409 this code is written to produce. That
// is an operator-facing regression, not a correctness one. There is
// deliberately no retry for it: the case is unreachable at the current cap,
// and speculative machinery for a configuration we do not run would rot.
//
// Rolling the write back on collision is safe: no other transaction is open
// while ours holds the pool's only connection, so nothing outside this
// transaction ever observes the write.
//
// Returns a non-nil *NameCollision when the rename was refused (nothing was
// written). The bool reports whether a real write happened, mirroring
// UpdateField: false with a nil collision means the no-op skip fired because
// the stored name already equals the requested one.
func (s *Service) UpdateNameGuarded(ctx context.Context, artistID, newName string) (*NameCollision, bool, error) {
	// The VALUE is refused before anything else. The collision check below
	// deliberately treats an empty identity key as "not a collision" (see
	// FindNameCollision), so without this the guard would wave a blanking
	// write straight through -- the guard's own exemption becoming the hole.
	// Validated HERE and not only in UpdateField because this method is also
	// called directly (handleFieldUpdate's transactional path), and a rule
	// that only one of two entry points enforces is the shape of defect #3037
	// exists to close.
	if err := ValidateFieldUpdate(string(FieldArtistName), newName); err != nil {
		return nil, false, err
	}

	db, err := s.artistDB()
	if err != nil {
		return nil, false, fmt.Errorf("guarded rename: %w", err)
	}
	if db == nil {
		return nil, false, fmt.Errorf("guarded rename: nil database handle")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("guarded rename: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit success is a no-op; on the error path the original error is what callers act on

	var currentName string
	if err := tx.QueryRowContext(ctx,
		`SELECT name FROM artists WHERE id = ?`, artistID).Scan(&currentName); err != nil {
		// A row that is simply ABSENT is reported as ErrNotFound so callers can
		// tell "this artist does not exist" (a 404) from "the rename could not
		// be decided" (a 500).
		//
		// WHICH CALLER ACTUALLY CONSUMES THIS, stated narrowly because the
		// obvious wider claim is false. The REVERT path does: Service.UpdateField
		// routes name writes here (#3037), and writeRevertFailure
		// (internal/api/handlers_history.go) branches on artist.ErrNotFound to
		// answer 404. The direct field-edit path does NOT -- handleFieldUpdate
		// turns every error from this method into a 500. It never observes this
		// mapping anyway: for the "name" field it runs guardNameCollision FIRST,
		// which loads the same missing artist through FindNameCollision, fails
		// closed and writes a 500 before this method is reached. So a 404 branch
		// added at that call site today would be dead code; the 500 is produced
		// upstream and predates #3037.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("guarded rename: loading artist %s: %w", artistID, ErrNotFound)
		}
		return nil, false, fmt.Errorf("guarded rename: loading artist %s: %w", artistID, err)
	}

	// Same no-op contract as UpdateField: an unchanged value writes nothing
	// and records no history. normalizeFieldValue compares scalars verbatim,
	// so a whitespace-correcting edit is still a real write.
	if normalizeFieldValue("name", newName) == normalizeFieldValue("name", currentName) {
		return nil, false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE artists SET name = ?, updated_at = ? WHERE id = ?`,
		newName, time.Now().UTC().Format(time.RFC3339), artistID); err != nil {
		return nil, false, fmt.Errorf("guarded rename: writing name: %w", err)
	}

	// Two cases are NOT collisions, matching FindNameCollision exactly: an
	// empty key and a new name whose key equals the artist's OWN current key
	// ("The Cure" -> "Cure" is cosmetic, not a second identity).
	//
	// The empty-key branch is now DEAD on this path -- the ValidateFieldUpdate
	// call at the top of this method already refused a name that normalizes to
	// "" -- and is kept only so the two scans stay literally identical, which
	// is the property that keeps the fast path and the authority from
	// disagreeing about what a duplicate is.
	newKey := NormalizeIdentityKey(newName)
	if newKey != "" && NormalizeIdentityKey(currentName) != newKey {
		collision, err := findCollisionPartner(ctx, tx, artistID, newKey)
		if err != nil {
			return nil, false, fmt.Errorf("guarded rename: %w", err)
		}
		if collision != nil {
			// The deferred Rollback discards the write above.
			return collision, false, nil
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("guarded rename: committing: %w", err)
	}

	s.markDirtyBestEffort(ctx, artistID)

	if s.history != nil {
		if err := s.history.Record(ctx, artistID, "name", currentName, newName, sourceFromContext(ctx)); err != nil {
			slog.Warn("history: failed to record guarded rename",
				"artist_id", artistID, "error", err)
		}
	}

	return nil, true, nil
}

// updateNameThroughGuard is what Service.UpdateField does when the field is
// "name". It exists so the collision guard cannot be BYPASSED by choosing a
// different single-field service method (#3037).
//
// THE DEFECT IT CLOSES. Service.UpdateField writes the name column directly
// through fieldColumnMap, so it never reaches UpdateNameGuarded and no
// collision check runs. Undoing a rename that had been applied to DE-DUPLICATE
// two artists would then recreate the duplicate the rename existed to remove
// -- exactly the state #2730 and #2807 exist to prevent.
//
// IS THAT OPERATOR-REACHABLE TODAY? YES, as of #3037's trackableFields change,
// and this route is what makes it safe. The production caller that drives it is
// the history revert (performRevert, internal/api/handlers_history.go), which
// validateRevertable gates on trackableFields membership. "name" was absent
// from that list when this route landed, so the routing closed the gap by
// CONSTRUCTION before the affordance existed; the same change that added "name"
// and "sort_name" to trackableFields then opened the door onto an already-locked
// write path. Checked, not assumed:
// `sed -n '/^var trackableFields/,/^}/p' internal/artist/service.go`
// -- so an Undo of a name change now reaches UpdateField and takes the
// transactional collision guard rather than the raw fieldColumnMap write. That
// is the order the repo wants: the guard lands first, so turning the affordance
// on was a one-line change rather than one that also had to remember to gate a
// write path.
//
// THE FIX IS THE ROUTE, NOT A CHECK AT THE CALLER. Patching performRevert
// would leave the next caller of UpdateField("name", ...)
// unguarded, and this repo has been bitten by exactly that shape before
// (#2748/#2754 gated six surfaces one at a time). Routing inside the service
// closes the SINGLE-FIELD bypass: every caller that asks UpdateField (or
// ClearField) to write "name" gets the transactional guard whether or not it
// knows the guard exists.
//
// WHAT THE ROUTE DOES NOT COVER, stated so the claim above is not read wider
// than it is true. Service.Update persists a WHOLE Artist struct and never
// passes through here, and Service.Create inserts one; a caller that assigns
// a.Name and calls either still writes the name column unguarded. Both predate
// #3037 and are enumerated in this file's header. "Every caller that writes
// name is guarded" is therefore FALSE as a general statement and true only of
// the single-field service methods.
//
// THE REFUSAL IS AN ERROR, NOT A SILENT FALSE. UpdateField's (false, nil)
// means "nothing needed doing", and callers render it as a benign no-op. A
// refused rename is the opposite of that, so it comes back as a
// *NameCollisionError carrying the colliding artist; callers that only need to
// classify it match errors.Is(err, ErrNameCollision).
func (s *Service) updateNameThroughGuard(ctx context.Context, id, value string) (bool, error) {
	collision, changed, err := s.UpdateNameGuarded(ctx, id, value)
	if err != nil {
		return false, err
	}
	if collision != nil {
		return false, &NameCollisionError{Collision: collision}
	}
	return changed, nil
}
