package rule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
)

// defaultImageDupTolerance is the perceptual similarity at or above which two
// images are reported as duplicates when the rule config supplies no valid
// tolerance of its own.
const defaultImageDupTolerance = 0.90

// imageDupMember describes one image involved in a detected duplicate pair.
// path is only populated for rows whose file had to be read this evaluation
// (to compute a hash that was not yet stored); it is empty for rows served
// entirely from stored hashes, which never need to be resolved to a file for
// detection to proceed.
type imageDupMember struct {
	imageType string
	slotIndex int
	slotName  string
	path      string
	// contentHash is the exact (sha256) hash of the file's bytes, or empty
	// when unknown. Empty never matches, including against another empty.
	contentHash string
}

// imageDupGroup is a single pair of images whose perceptual hashes meet or
// exceed the configured similarity tolerance.
type imageDupGroup struct {
	a, b       imageDupMember
	similarity float64
	// withinTypeFanart is true when both members are fanart images at
	// different slot indices. Only these groups are safely removable: the
	// higher-numbered slot is a redundant copy of the lower one and can be
	// deleted without losing any distinct artwork. Cross-type duplicates
	// (e.g. thumb vs fanart) are informational only -- resolving them
	// requires replacing one of the images with a distinct alternative,
	// which the fixer cannot decide on its own.
	withinTypeFanart bool
}

// resolveFanartPrimaryName returns the primary fanart filename from the
// active platform profile, falling back to the default naming convention
// when no profile is configured or active. Mirrors the pattern used by
// BackdropSequencingFixer so detection and sequencing agree on which files
// constitute the fanart set.
func resolveFanartPrimaryName(ctx context.Context, platformService *platform.Service) string {
	if platformService != nil {
		if profile, err := platformService.GetActive(ctx); err == nil && profile != nil {
			if names := profile.ImageNaming.NamesForType("fanart"); len(names) > 0 {
				return names[0]
			}
		}
	}
	if names := image.FileNamesForType(image.DefaultFileNames, "fanart"); len(names) > 0 {
		return names[0]
	}
	return ""
}

// imageSlotLabel formats a human-readable label for a (type, slot) pair used
// in violation messages, e.g. "fanart slot 0" or "thumb slot 0".
func imageSlotLabel(imageType string, slotIndex int) string {
	return fmt.Sprintf("%s slot %d", imageType, slotIndex)
}

// imageDupRawRow is one exists_flag=1 artist_images row as read from the DB,
// prior to hash resolution.
type imageDupRawRow struct {
	imageType   string
	slotIndex   int
	hashHex     string
	contentHash string
}

// queryImageDupRows loads every exists_flag=1 image row for the artist.
func queryImageDupRows(ctx context.Context, db *sql.DB, a *artist.Artist, logger *slog.Logger) ([]imageDupRawRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT image_type, slot_index, phash, content_hash FROM artist_images WHERE artist_id = ? AND exists_flag = 1`,
		a.ID)
	if err != nil {
		return nil, fmt.Errorf("querying image rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var raw []imageDupRawRow
	for rows.Next() {
		var r imageDupRawRow
		if scanErr := rows.Scan(&r.imageType, &r.slotIndex, &r.hashHex, &r.contentHash); scanErr != nil {
			logger.Debug("scanning image row for duplicate detection", "artist", a.Name, "error", scanErr)
			continue
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating image rows: %w", err)
	}
	return raw, nil
}

// imageHashRecorder persists hashes computed during duplicate detection back
// to artist_images. It is the narrow slice of artist.Service that the rule
// engine needs; see Engine.SetImageHashRecorder.
type imageHashRecorder interface {
	UpdateImageHashes(ctx context.Context, artistID, imageType string, slotIndex int, phash, contentHash string) error
}

// hashImageFile is the seam through which duplicate detection reads and hashes
// a file. It is a variable so tests can count reads and decodes and prove that
// the hashes are computed at most once per file rather than once per
// evaluation. Production code never reassigns it.
var hashImageFile = image.HashFile

// resolvedHashes is the outcome of hash resolution for a single image row.
type resolvedHashes struct {
	perceptual uint64
	content    string
	// path is the on-disk file, populated only when the row was resolved
	// from disk; rows served entirely from stored hashes never need it.
	path string
	// usable is false when neither hash could be obtained and the row must
	// be excluded from comparison entirely.
	usable bool
}

// resolveImageDupHashes returns the hashes for one image row, reading the file
// at most once and decoding it only when the perceptual hash is genuinely
// missing.
//
// This is the heart of the fix for the recomputation bug: previously every
// numbered fanart slot was re-opened and fully re-decoded on every single rule
// evaluation, because nothing ever wrote the computed hash back. Now a row is
// touched on disk only while one of its hashes is still empty; once both are
// persisted, evaluation is pure DB reads and does no filesystem or CPU work at
// all.
//
// Ordering matters within the read as well: the content hash is a sha256 over
// bytes that are already in memory, whereas the perceptual hash needs a full
// decode and resample. So the decode is requested only when the stored phash is
// absent, and it is skipped entirely for a file whose hashes are already known.
//
// fanartPaths is the already-discovered on-disk fanart path list indexed by
// slot. persist, when non-nil, writes any newly computed hash back so no later
// evaluation recomputes it.
func resolveImageDupHashes(
	ctx context.Context,
	a *artist.Artist,
	r imageDupRawRow,
	fanartPaths []string,
	persist imageHashRecorder,
	logger *slog.Logger,
) resolvedHashes {
	storedPerceptual, parseErr := image.ParseHashHex(r.hashHex)
	havePerceptual := parseErr == nil && storedPerceptual != 0
	haveContent := r.contentHash != ""

	if havePerceptual && haveContent {
		// Fully cached: the common steady-state path. No file is touched.
		return resolvedHashes{perceptual: storedPerceptual, content: r.contentHash, usable: true}
	}

	path, ok := imageDupRowPath(r, fanartPaths)
	if !ok {
		// Nothing to read. A stored perceptual hash on its own is still
		// usable for the perceptual tier; the row simply cannot take part
		// in exact matching (its content hash stays unknown, and unknown
		// must never be treated as "identical to other unknowns").
		return resolvedHashes{perceptual: storedPerceptual, usable: havePerceptual}
	}

	fh, err := hashImageFile(path, !havePerceptual)
	if err != nil {
		// A decode failure still leaves a valid content hash (an
		// undecodable file can be byte-compared), so fall through with
		// whatever was obtained rather than discarding the whole row.
		logger.Debug("hashing image slot for duplicate detection",
			"artist", a.Name, "path", path, "error", err)
	}

	res := resolvedHashes{perceptual: storedPerceptual, content: r.contentHash, path: path}
	if !havePerceptual && fh.Perceptual != 0 {
		res.perceptual = fh.Perceptual
	}
	if !haveContent && fh.Content != "" {
		res.content = fh.Content
	}
	res.usable = res.perceptual != 0 || res.content != ""

	// Persist whatever is now known so this file is never re-read for these
	// hashes again. Both columns are written together because UpdateHashes
	// sets both; passing the value already in hand for a column that did not
	// change is a no-op rewrite, not a clobber.
	if persist != nil && res.usable && (res.perceptual != storedPerceptual || res.content != r.contentHash) {
		phashHex := ""
		if res.perceptual != 0 {
			phashHex = image.HashHex(res.perceptual)
		}
		if err := persist.UpdateImageHashes(ctx, a.ID, r.imageType, r.slotIndex, phashHex, res.content); err != nil {
			// A vanished slot means a concurrent scan renumbered or removed
			// it between the SELECT and here. Detection for this run is
			// still correct; the next run re-derives and re-persists.
			if errors.Is(err, artist.ErrNotFound) {
				logger.Debug("image slot vanished before hash persist",
					"artist", a.Name, "type", r.imageType, "slot", r.slotIndex)
			} else {
				logger.Warn("persisting image hashes for duplicate detection",
					"artist", a.Name, "type", r.imageType, "slot", r.slotIndex, "error", err)
			}
		}
	}

	return res
}

// imageDupRowPath maps a DB row to its on-disk file. Only fanart resolves,
// because DiscoverFanart is the sole slot-to-path mapping available here, and
// fanart is the only multi-slot image type -- which is also the only place a
// within-type duplicate can exist. Single-slot types (thumb, logo, banner) rely
// on the phash written at save time.
func imageDupRowPath(r imageDupRawRow, fanartPaths []string) (string, bool) {
	if r.imageType != "fanart" || r.slotIndex < 0 || r.slotIndex >= len(fanartPaths) {
		return "", false
	}
	// DiscoverFanart returns paths in ascending slot order (see
	// BackdropSequencingFixer), so slot_index indexes the slice directly.
	return fanartPaths[r.slotIndex], true
}

// pairImageDuplicates compares every pair of members and returns the groups
// whose similarity meets or exceeds tolerance.
//
// Byte-identical pairs are deliberately NOT filtered out here even though the
// exact rule also reports them. The two rules have independent enable toggles,
// and the perceptual rule can legitimately run with the exact rule switched
// off; suppressing byte-identical pairs from this tier would leave that
// configuration unable to see them at all, turning the most obvious kind of
// duplicate into the one kind nothing detects. Reporting a duplicate under
// both rules when both are enabled is redundant but honest, and their fixers
// converge on the same end state: the exact fixer removes the file first, and
// the perceptual fixer re-detects from disk before acting, so it finds nothing
// left to do rather than deleting twice.
//
// The perceptual pass is made cheaper by persisting hashes (so nothing is
// re-decoded) and by the exact fixer actually removing files (so later
// evaluations have fewer images to compare) -- not by hiding rows from it.
func pairImageDuplicates(members []imageDupMember, hashes []uint64, tolerance float64) []imageDupGroup {
	var groups []imageDupGroup
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if hashes[i] == 0 || hashes[j] == 0 {
				// A member can reach this point with a content hash but no
				// usable perceptual hash (an image that would not decode).
				// Similarity(0, 0) is 1.0, so comparing those would report
				// two undecodable images as identical; only the exact tier
				// can speak to them.
				continue
			}
			sim := image.Similarity(hashes[i], hashes[j])
			if sim < tolerance {
				continue
			}
			withinType := members[i].imageType == "fanart" &&
				members[j].imageType == "fanart" &&
				members[i].slotIndex != members[j].slotIndex
			groups = append(groups, imageDupGroup{
				a:                members[i],
				b:                members[j],
				similarity:       sim,
				withinTypeFanart: withinType,
			})
		}
	}
	return groups
}

// exactFanartDuplicates groups fanart slots by content hash and returns, for
// each group of byte-identical files, the slots that may be deleted -- every
// slot except the lowest, which is kept as the canonical copy.
//
// Byte equality is transitive, unlike perceptual similarity, so this needs
// none of the representative-walking that nonTransitiveFanartDeletionSet has to
// do: if a == b and b == c then a == c, and the whole group collapses onto its
// lowest slot with no risk of destroying distinct artwork.
func exactFanartDuplicates(members []imageDupMember) map[int]bool {
	bySlot := make(map[string][]int)
	for _, m := range members {
		if m.imageType != "fanart" || m.contentHash == "" {
			continue
		}
		bySlot[m.contentHash] = append(bySlot[m.contentHash], m.slotIndex)
	}

	toDelete := make(map[int]bool)
	for _, slots := range bySlot {
		if len(slots) < 2 {
			continue
		}
		sort.Ints(slots)
		for _, s := range slots[1:] {
			toDelete[s] = true
		}
	}
	return toDelete
}

// imageDupResult carries both duplicate tiers from a single detection pass, so
// that the exact and perceptual rules share one query, one directory scan, and
// one read per file rather than each paying for their own.
type imageDupResult struct {
	// perceptual holds pairs that are visually similar but NOT byte-identical
	// (byte-identical pairs are the exact tier's, and are excluded here).
	perceptual []imageDupGroup
	// exactFanartToDelete is the set of fanart slots that are byte-identical
	// to a lower-numbered slot and can be removed with no false positives.
	exactFanartToDelete map[int]bool
	// members is every row that yielded at least one usable hash.
	members []imageDupMember
}

// findImageDuplicates enumerates an artist's exists_flag=1 image rows and
// detects duplicates in two tiers from a single pass over the data.
//
// The exact tier compares sha256 content hashes: cheap (no decode), transitive,
// and free of false positives, so its matches are safe to remove automatically.
// The perceptual tier compares dHash similarity, which additionally catches
// re-encoded or retagged copies of the same picture, but is only a similarity
// judgement and so stays manual.
//
// The tiers are ordered exact-first for cost, and they are computed together on
// purpose. Running them as two sweeps would read every image twice, giving back
// most of what the cheap-first ordering buys; instead each file is read at most
// once (see resolveImageDupHashes), its content hash always taken, and a decode
// performed only when the perceptual hash is missing.
//
// Ordering is a filter, not the fix, for the recomputation problem: exact-first
// shrinks how many images reach the perceptual comparison, but the survivors
// would still be re-decoded on every evaluation if nothing persisted their
// hash. Persistence is what makes hashing a once-per-file cost, and it is why
// this function takes a recorder and is no longer strictly read-only. It writes
// only to the hash columns of rows it just read, which is idempotent and
// invisible to callers: neither the violation set nor any file on disk changes
// as a result, so the checker still observes the Checker contract of not
// mutating the artist's state.
func findImageDuplicates(
	ctx context.Context,
	db *sql.DB,
	a *artist.Artist,
	fanartPrimaryName string,
	tolerance float64,
	persist imageHashRecorder,
	logger *slog.Logger,
) (imageDupResult, error) {
	var out imageDupResult
	if a.Path == "" || db == nil {
		return out, nil
	}

	raw, err := queryImageDupRows(ctx, db, a, logger)
	if err != nil {
		return out, err
	}

	fanartPaths := discoverFanartForDup(a, raw, fanartPrimaryName, logger)

	var hashes []uint64
	for _, r := range raw {
		res := resolveImageDupHashes(ctx, a, r, fanartPaths, persist, logger)
		if !res.usable {
			continue
		}
		out.members = append(out.members, imageDupMember{
			imageType:   r.imageType,
			slotIndex:   r.slotIndex,
			slotName:    imageSlotLabel(r.imageType, r.slotIndex),
			path:        res.path,
			contentHash: res.content,
		})
		hashes = append(hashes, res.perceptual)
	}

	out.exactFanartToDelete = exactFanartDuplicates(out.members)
	out.perceptual = pairImageDuplicates(out.members, hashes, tolerance)
	return out, nil
}

// discoverFanartForDup resolves the artist's on-disk fanart paths, but only
// when some fanart row actually needs a file read (a hash it does not have
// stored). An artist whose hashes are all persisted causes no directory scan
// at all, which is the steady state after the first evaluation.
func discoverFanartForDup(a *artist.Artist, raw []imageDupRawRow, fanartPrimaryName string, logger *slog.Logger) []string {
	if fanartPrimaryName == "" {
		return nil
	}
	needed := false
	for _, r := range raw {
		if r.imageType != "fanart" {
			continue
		}
		storedPerceptual, parseErr := image.ParseHashHex(r.hashHex)
		if r.contentHash == "" || parseErr != nil || storedPerceptual == 0 {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}

	discovered, discErr := image.DiscoverFanart(a.Path, fanartPrimaryName)
	if discErr != nil {
		logger.Debug("discovering fanart for duplicate detection", "artist", a.Name, "error", discErr)
		return nil
	}
	return discovered
}

// nonTransitiveFanartDeletionSet returns the set of within-type fanart slot
// indices that ImageDuplicateFixer.Fix may safely delete.
//
// Perceptual-hash similarity is pairwise, not transitive: sim(0,1) and
// sim(1,2) can both clear tolerance while sim(0,2) does not, meaning slots 0
// and 2 hold genuinely distinct artwork even though slot 1 resembles both
// (slot 1 might be, say, a blurred or slightly cropped variant that happens
// to sit hash-wise between two unrelated images). Naively deleting the
// higher-numbered member of every detected pair would treat that as one
// transitive group spanning slots 0-2 and destroy slot 2's distinct
// artwork, violating the "without losing any distinct artwork" invariant
// (see imageDupGroup.withinTypeFanart doc comment).
//
// This walks slots in ascending order. Each slot not already marked for
// deletion becomes a "representative" that survives. A higher slot is
// marked for deletion only when it is directly paired (in the same
// detected group, not via a chain through some other slot) with a
// surviving representative -- never with a slot that is itself already
// marked for removal. In the sim(0,1)+sim(1,2)-but-not-sim(0,2) scenario
// above, slot 0 is a representative and deletes slot 1 (direct pair), but
// slot 2 is never directly paired with slot 0, so it survives and becomes
// its own representative.
func nonTransitiveFanartDeletionSet(groups []imageDupGroup) map[int]bool {
	pairSet := make(map[[2]int]bool)
	slotSet := make(map[int]bool)
	for i := range groups {
		g := &groups[i]
		if !g.withinTypeFanart {
			continue
		}
		lo, hi := g.a.slotIndex, g.b.slotIndex
		if lo > hi {
			lo, hi = hi, lo
		}
		pairSet[[2]int{lo, hi}] = true
		slotSet[lo] = true
		slotSet[hi] = true
	}

	slots := make([]int, 0, len(slotSet))
	for s := range slotSet {
		slots = append(slots, s)
	}
	sort.Ints(slots)

	toDelete := make(map[int]bool)
	for _, i := range slots {
		if toDelete[i] {
			continue
		}
		for _, j := range slots {
			if j <= i || toDelete[j] {
				continue
			}
			if pairSet[[2]int{i, j}] {
				toDelete[j] = true
			}
		}
	}
	return toDelete
}
