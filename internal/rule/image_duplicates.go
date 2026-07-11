package rule

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
)

// imageDupMember describes one image involved in a detected duplicate pair.
// path is only populated for numbered fanart slots (slot_index > 0) whose
// hash was computed on demand from disk; it is empty for rows that carry a
// stored phash (slot 0 of every type), since those never need to be resolved
// to a file for the fixer to act on.
type imageDupMember struct {
	imageType string
	slotIndex int
	slotName  string
	path      string
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
	imageType string
	slotIndex int
	hashHex   string
}

// queryImageDupRows loads every exists_flag=1 image row for the artist.
func queryImageDupRows(ctx context.Context, db *sql.DB, a *artist.Artist, logger *slog.Logger) ([]imageDupRawRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT image_type, slot_index, phash FROM artist_images WHERE artist_id = ? AND exists_flag = 1`,
		a.ID)
	if err != nil {
		return nil, fmt.Errorf("querying image rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var raw []imageDupRawRow
	for rows.Next() {
		var r imageDupRawRow
		if scanErr := rows.Scan(&r.imageType, &r.slotIndex, &r.hashHex); scanErr != nil {
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

// resolveImageDupHash returns the perceptual hash for one raw row, using the
// stored phash when valid, or computing it on demand from disk for numbered
// fanart slots that have none (see findImageDuplicates doc comment). ok is
// false when no usable hash could be obtained and the row should be skipped.
// fanartPaths is the (already discovered) on-disk fanart path list, indexed
// by slot; path is only populated when the hash was computed on demand.
func resolveImageDupHash(a *artist.Artist, r imageDupRawRow, fanartPaths []string, logger *slog.Logger) (h uint64, path string, ok bool) {
	if hv, parseErr := image.ParseHashHex(r.hashHex); parseErr == nil && hv != 0 {
		return hv, "", true
	}
	if r.imageType != "fanart" || r.slotIndex <= 0 {
		// No usable stored hash and not a numbered fanart slot we can
		// compute on demand (empty, zero-sentinel, or unparsable phash).
		return 0, "", false
	}

	// Numbered fanart slots have no stored phash; resolve the file on disk
	// and hash it on demand. DiscoverFanart returns paths in ascending slot
	// order (see BackdropSequencingFixer), so the slot_index is a direct
	// index into the discovered slice.
	if r.slotIndex >= len(fanartPaths) {
		return 0, "", false
	}
	path = fanartPaths[r.slotIndex]
	f, openErr := os.Open(path) //nolint:gosec // path resolved from DiscoverFanart within the artist's own library directory
	if openErr != nil {
		logger.Debug("opening fanart slot for duplicate hash", "artist", a.Name, "path", path, "error", openErr)
		return 0, "", false
	}
	computed, hashErr := image.PerceptualHash(f)
	_ = f.Close()
	if hashErr != nil {
		logger.Debug("hashing fanart slot for duplicate detection", "artist", a.Name, "path", path, "error", hashErr)
		return 0, "", false
	}
	return computed, path, true
}

// pairImageDuplicates compares every pair of members and returns the groups
// whose similarity meets or exceeds tolerance.
func pairImageDuplicates(members []imageDupMember, hashes []uint64, tolerance float64) []imageDupGroup {
	var groups []imageDupGroup
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
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

// findImageDuplicates enumerates an artist's exists_flag=1 image rows across
// all types and slots and returns every pair whose perceptual hashes meet or
// exceed tolerance. Rows with a valid stored phash use it directly. Numbered
// fanart slots (slot_index > 0) are persisted with no stored phash, so their
// hash is computed on demand by mapping the slot to its on-disk file via
// image.DiscoverFanart and reading it. Files that cannot be opened or
// decoded are skipped. Detection is strictly read-only: no DB or filesystem
// writes are made here, so both the checker and the fixer can call it
// without violating the Checker contract.
func findImageDuplicates(ctx context.Context, db *sql.DB, a *artist.Artist, fanartPrimaryName string, tolerance float64, logger *slog.Logger) ([]imageDupGroup, error) {
	if a.Path == "" || db == nil {
		return nil, nil
	}

	raw, err := queryImageDupRows(ctx, db, a, logger)
	if err != nil {
		return nil, err
	}

	// Discover on-disk fanart paths only if a numbered fanart slot is
	// actually present, avoiding an unnecessary directory read for artists
	// with none.
	var fanartPaths []string
	needsFanartPaths := false
	for _, r := range raw {
		if r.imageType == "fanart" && r.slotIndex > 0 {
			needsFanartPaths = true
			break
		}
	}
	if needsFanartPaths && fanartPrimaryName != "" {
		discovered, discErr := image.DiscoverFanart(a.Path, fanartPrimaryName)
		if discErr != nil {
			logger.Debug("discovering fanart for duplicate detection", "artist", a.Name, "error", discErr)
		} else {
			fanartPaths = discovered
		}
	}

	var members []imageDupMember
	var hashes []uint64
	for _, r := range raw {
		h, path, ok := resolveImageDupHash(a, r, fanartPaths, logger)
		if !ok {
			continue
		}
		members = append(members, imageDupMember{
			imageType: r.imageType,
			slotIndex: r.slotIndex,
			slotName:  imageSlotLabel(r.imageType, r.slotIndex),
			path:      path,
		})
		hashes = append(hashes, h)
	}

	return pairImageDuplicates(members, hashes, tolerance), nil
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
