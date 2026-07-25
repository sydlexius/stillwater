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
// Enforcement lives at the API boundary (handleFieldUpdate), not inside
// Service.UpdateField. UpdateField is also driven by the history-revert and
// platform-state sync paths, which must stay able to restore a prior value;
// turning the service method itself into a hard gate would break those flows.
// So this file supplies the DETECTION and the API layer decides the policy.

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// FindNameCollision reports whether renaming artistID to newName would land it
// on an identity key that a different artist already holds.
//
// Returns (nil, nil) when the rename is safe. A non-nil *NameCollision means
// the write must not proceed. A non-nil error means the check could not be
// completed -- callers MUST treat that as a failure and refuse the write
// rather than falling through to the unguarded path, since "the guard could
// not run" is not evidence that the rename is safe.
//
// Two cases are deliberately NOT collisions:
//
//   - An empty normalized key (newName is blank or punctuation-only). Field
//     validation rejects those separately; reporting a collision here would
//     produce a misleading message.
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

	// Scan every OTHER artist and compare normalized keys in Go. The key is
	// computed by a Go function (Unicode normalization, punctuation folding,
	// article stripping), so it cannot be expressed as a SQL predicate and
	// there is no stored key column to index. This is the same whole-table
	// approach DetectDuplicates takes, at the same scale (one row per artist),
	// and it runs once per name edit -- a rare, human-driven action.
	//
	// ORDER BY makes the reported partner deterministic when more than one
	// existing artist shares the key, so the operator sees a stable message
	// instead of one that changes between identical attempts. Pinned by
	// TestFindNameCollision_MultiplePartnersIsDeterministic, which seeds two
	// artists sharing the target key with IDs chosen so a different ordering
	// would select the other row.
	const q = `SELECT id, name, path FROM artists WHERE id <> ? ORDER BY name, id`
	rows, err := db.QueryContext(ctx, q, artistID)
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
