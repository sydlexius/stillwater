package image

import (
	"context"
	"fmt"
	"strings"
)

// ResolveFanartFiles returns an artist directory's fanart files in
// DiscoverFanart ordinal order, resolving which naming convention applies the
// same way the scanner's discoverFanartFiles does, in two passes.
//
// Pass 1: the first candidate whose PRIMARY file is on disk wins, and
// enumeration runs from that name. Pass 2 runs only when pass 1 found nothing,
// and accepts orphan numbered variants -- fanart1.jpg with no fanart.jpg.
//
// Pass 2 is why this function exists in the repair path. That shape is exactly
// what a slot delete which failed partway leaves behind (#2635, #2644), and it
// is the state the scanner used to report as "no fanart at all". A repair that
// only ran pass 1 would walk past the very artists whose rows were destroyed.
//
// candidates must be the profile-INDEPENDENT superset (DefaultFileNames
// ["fanart"]), never the active profile's primary name. The scanner resolves
// against the superset (internal/scanner/scanner.go fanartPatterns), and any
// caller that writes registry rows has to agree with it byte for byte: the
// scanner owns the delete path, so a row derived from a different candidate
// order is a row the next scan removes.
//
// A directory that cannot be read returns an error. It never returns
// (nil, nil) for an unreadable directory -- "cannot tell" and "no fanart
// here" are different facts, and collapsing them is what destroyed the
// registry rows this function exists to help rebuild.
//
// ctx bounds every directory read this performs (#2689). It sits directly on
// the registry-repair rebuild path, inside the registry-repair job, whose singleton a wedged
// run never releases.
func ResolveFanartFiles(ctx context.Context, dir string, candidates []string) ([]string, error) {
	_, paths, err := ResolveFanartFilesNamed(ctx, dir, candidates)
	return paths, err
}

// ResolveFanartFilesNamed is ResolveFanartFiles plus the candidate pattern that
// matched ("" when nothing matched). It is the ONE implementation of the
// scanner-parity resolution: ResolveFanartFiles wraps it, so the two cannot
// drift. A caller that must renumber under the matched convention
// (RenumberFanart) needs the name; a caller that only needs paths uses the
// wrapper.
func ResolveFanartFilesNamed(ctx context.Context, dir string, candidates []string) (string, []string, error) {
	entries, err := readDirCtx(ctx, dir)
	if err != nil {
		return "", nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	// Case-insensitive PRESENCE index. It answers one question -- "is this
	// candidate's primary on disk" -- and is never used to decide WHICH file
	// wins an ordinal.
	//
	// That distinction is the point. Keying on strings.ToLower collapses names
	// that differ only in case, so a map that carried the on-disk name forward
	// would hand the next stage whichever entry ReadDir happened to insert
	// last. slot_index is a DiscoverFanart ordinal and ordinal 0's path is what
	// gets probed for dimensions, so that choice is load-bearing. Here the
	// collapse is harmless because the value is discarded: the winning
	// CANDIDATE PATTERN is passed on, exactly as the scanner passes its pattern
	// after an EqualFold match, and every ordinal decision is made by
	// DiscoverFanart walking the raw directory itself.
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			present[strings.ToLower(e.Name())] = true
		}
	}

	// Both passes match against the SAME entries snapshot (DiscoverFanartFrom
	// does no I/O), so the chosen candidate and the ordinal list can never come
	// from two different directory states.
	//
	// Pass 1: a primary file on disk decides which naming convention applies.
	for _, candidate := range candidates {
		if present[strings.ToLower(candidate)] {
			return candidate, DiscoverFanartFrom(dir, entries, candidate), nil
		}
	}

	// Pass 2: no primary anywhere, so numbered variants may still be present.
	// Strictly additive -- it cannot change any artist pass 1 resolved.
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		paths := DiscoverFanartFrom(dir, entries, candidate)
		if len(paths) > 0 {
			return candidate, paths, nil
		}
	}
	return "", nil, nil
}

// FanartSlotsContiguous reports whether slots (the slot_index of every live
// fanart row for one artist) are exactly {0..n-1}, where n is the number of
// fanart files on disk. Rows map to files by POSITION (slot_index indexes the
// ordinal list), so that mapping is only sound when every ordinal has exactly
// one row. A count match alone is not enough: rows 0, 2, 3 against three files
// pass a count check yet pair slot 2 with the second file. Duplicate slots
// cannot occur (UNIQUE(artist_id, image_type, slot_index)), but this checks
// the set anyway rather than lean on that.
func FanartSlotsContiguous(slots []int, n int) bool {
	if len(slots) != n {
		return false
	}
	seen := make([]bool, n)
	for _, s := range slots {
		if s < 0 || s >= n || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
