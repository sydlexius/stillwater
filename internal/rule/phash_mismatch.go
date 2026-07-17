// Package rule -- phash_mismatch.go
//
// Read-only detection of cross-artist backdrop pollution (#2564 PR-2): a
// fanart slot bound to artist A whose picture is actually artist B's.
//
// # Why the signal is fanart-to-fanart, not fanart-to-thumb
//
// The obvious detector -- compare each fanart slot against the artist's own
// thumb and flag it when they disagree -- cannot work, and the reason it
// cannot work also kills its cross-artist variant (flag a slot when it
// collides with some OTHER artist's thumb). A legitimate backdrop is a wide
// promo shot; a thumb is a portrait headshot. They are different photographs
// of the same person, so they do not collide perceptually. If artist A's
// CORRECT backdrop is naturally dissimilar to A's own thumb, then a POLLUTED
// backdrop in A's slots -- which is artist B's backdrop -- is equally
// dissimilar to B's thumb. A thumb registry therefore matches approximately
// nothing and reports "0 suspects, all clean" over a badly polluted library.
//
// The arithmetic is unambiguous. Similarity is 1 - Hamming/64
// (internal/image/hash.go), so the 0.90 tolerance demands a Hamming distance
// of at most 6 bits out of 64 -- a strict near-duplicate. Distinct
// photographs sit 20-30+ bits apart. A thumb-based detector is a confident
// false-green in a data-loss tool.
//
// What DOES collide is the same photograph. The polluting image is a
// fanart.tv image belonging to artist B, so if B is in the library, B's own
// fanart slots very likely hold that same source image -- a genuine
// near-duplicate, well inside the tolerance. That is the primary signal here:
// cross-artist fanart-to-fanart perceptual collision.
//
// # What this detector does NOT tell you
//
// A collision is symmetric. It proves that artists A and B share a picture;
// it does not say which of them owns it. Both sides are reported, with the
// similarity score, and a human decides -- that is what the dry run, the
// per-suspect score, the admin confirmation and the quarantine exist to
// absorb. Two artists can also legitimately share a promo image (a duo, a
// collaboration, a festival shot), which lands here as a true collision and a
// false pollution report.
//
// The thumb registry is kept only as CORROBORATING ATTRIBUTION: on the rare
// occasion a polluting image really is close to some artist's thumb, saying
// whose thumb it is names the true subject, which is useful in the report. It
// never raises a suspect on its own.
//
// # Absence of evidence
//
// Neither signal detects pollution whose source artist is absent from the
// library: nothing in the library holds a second copy of that picture, so
// there is nothing to collide with. This report can therefore never be read
// as proof of cleanliness. "0 suspects" means "0 found", not "clean", and the
// report is shaped to say so: slots that could not be evaluated are counted
// separately from slots that were evaluated and passed, and registry coverage
// (indexed vs skipped) is reported so a silently under-built registry cannot
// masquerade as a clean library.
package rule

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// PHashMismatchScope narrows a scan.
type PHashMismatchScope struct {
	// ArtistID, when set, probes only that artist's fanart slots. The
	// registry is still built from the WHOLE library: a scoped scan asks
	// "does this artist hold anyone else's picture", which requires
	// everyone else's pictures to compare against.
	ArtistID string

	// Tolerance is the minimum perceptual similarity (0, 1] at which two
	// fanart slots are reported as holding the same picture. Zero selects
	// defaultPHashMismatchTolerance.
	Tolerance float64
}

// defaultPHashMismatchTolerance is the similarity at or above which two fanart
// slots are reported as holding the same photograph.
//
// It is deliberately the same value as defaultImageDupTolerance (0.90), but it
// is DERIVED here rather than inherited: that constant was chosen to answer "is
// this the same image?" for duplicate reporting, and this detector feeds a path
// that deletes files, so reusing a number without restating why it holds would
// be exactly the kind of borrowed threshold that goes wrong quietly.
//
// The derivation: Similarity = 1 - Hamming/64, so 0.90 admits a match only
// within 6 differing bits of a 64-bit dHash. That is the near-duplicate band --
// re-encodes, rescales, and requantizations of ONE photograph, which is
// precisely the relationship between a polluting fanart.tv image in artist A's
// slots and the same fanart.tv image in artist B's slots. Two different
// photographs, even of the same subject in the same session, land far outside
// it (typically 20+ bits, i.e. below 0.70). So the question this detector asks
// ("are these two slots the same picture?") is the same question the constant
// was derived for, on the same hash, at the same scale -- the reuse is
// in-context rather than a coincidence of magnitude.
//
// It is operator-configurable per scan (PHashMismatchScope.Tolerance) because
// the correct operating point is a library property: raising it toward 1.0
// admits only near-exact copies (fewer false positives, more missed
// pollution), lowering it widens the net. The per-suspect similarity score is
// reported so an operator can see where each match actually landed rather than
// trusting the cutoff.
const defaultPHashMismatchTolerance = defaultImageDupTolerance

// scanPHashPageSize bounds each artist-list page. Must be within
// artist.ListParams.Validate's [10, 500] range or it is silently clamped.
const scanPHashPageSize = 200

// PHashCollision is one fanart slot that holds the same picture as some other
// artist's fanart slot.
type PHashCollision struct {
	SlotIndex int    `json:"slot_index"`
	PHash     string `json:"phash"`

	// MatchedArtistID/Name/SlotIndex identify the other side of the
	// collision -- NOT "the artist this image belongs to". The collision is
	// symmetric; this side is reported for that side too.
	MatchedArtistID   string `json:"matched_artist_id"`
	MatchedArtistName string `json:"matched_artist_name"`
	MatchedSlotIndex  int    `json:"matched_slot_index"`

	// Similarity is the score of the best match, reported so a human can
	// judge each suspect rather than trusting the tolerance.
	Similarity float64 `json:"similarity"`

	// MatchCount is how many other-artist fanart slots this slot collided
	// with. A picture that collides with many artists is more likely to be
	// shared legitimate promo art than a single wrong-artist write.
	MatchCount int `json:"match_count"`

	// ThumbAttributionArtistID/Name are the CORROBORATING signal: an artist
	// whose thumb this picture also resembles, which names the true subject
	// when it fires. Empty is the common case and means nothing.
	ThumbAttributionArtistID   string `json:"thumb_attribution_artist_id,omitempty"`
	ThumbAttributionArtistName string `json:"thumb_attribution_artist_name,omitempty"`
}

// PHashIndeterminateSlot is an in-scope fanart slot that exists on disk but
// carries no usable perceptual hash, so it could NOT be evaluated. It is not
// clean; it is unknown.
type PHashIndeterminateSlot struct {
	ArtistID   string `json:"artist_id"`
	ArtistName string `json:"artist_name"`
	SlotIndex  int    `json:"slot_index"`
	Reason     string `json:"reason"`
}

// ArtistPHashMismatch is one artist's suspect slots.
type ArtistPHashMismatch struct {
	ArtistID string           `json:"artist_id"`
	Name     string           `json:"name"`
	Suspects []PHashCollision `json:"suspects"`
}

// PHashRegistryCoverage reports how much of the library the comparison
// registry actually covers. An artist contributing no usable fanart hash is
// invisible to every other artist's scan: their pictures cannot be recognized
// as pollution anywhere. That gap is reported rather than absorbed, because a
// silently under-built registry produces the same "0 suspects" as a clean
// library.
type PHashRegistryCoverage struct {
	// ArtistsIndexed counts artists that contributed at least one usable
	// hash. ArtistsSkipped counts KNOWN artists that contributed none --
	// including artists with no artist_images row at all, which are just as
	// invisible to every other artist's scan as an artist whose rows carry
	// no usable hash. Together they cover the whole artist list, so
	// indexed+skipped is the library size rather than only the subset that
	// happened to have rows.
	ArtistsIndexed int `json:"artists_indexed"`
	ArtistsSkipped int `json:"artists_skipped"`

	// SlotsIndexed/SlotsSkipped count individual artist_images rows, so an
	// artist with no rows contributes to neither.
	SlotsIndexed int `json:"slots_indexed"`
	SlotsSkipped int `json:"slots_skipped"`
}

// PHashMismatchReport is the library-wide detection result.
type PHashMismatchReport struct {
	Tolerance       float64 `json:"tolerance"`
	ScopedArtistID  string  `json:"scoped_artist_id,omitempty"`
	ArtistsScanned  int     `json:"artists_scanned"`
	ArtistsAffected int     `json:"artists_affected"`

	// SlotsEvaluated counts in-scope fanart slots that carried a usable
	// hash and were actually compared. SuspectSlots of them collided; the
	// remainder were evaluated and did not. This is deliberately NOT
	// labeled "clean": see the package comment on absent source artists.
	SlotsEvaluated int `json:"slots_evaluated"`
	SuspectSlots   int `json:"suspect_slots"`

	// IndeterminateSlots is the could-not-evaluate bucket, held apart from
	// SlotsEvaluated so a data-starved scan can never read as a passing one.
	IndeterminateSlots int `json:"indeterminate_slots"`

	FanartRegistry PHashRegistryCoverage `json:"fanart_registry"`
	ThumbRegistry  PHashRegistryCoverage `json:"thumb_registry"`

	PerArtist     []ArtistPHashMismatch    `json:"per_artist"`
	Indeterminate []PHashIndeterminateSlot `json:"indeterminate"`

	// ScanErrors counts rows dropped by a read failure, surfaced so a
	// partial scan is never mistaken for a complete one.
	ScanErrors int `json:"scan_errors"`
}

// phashEntry is one hashed fanart slot in the comparison registry.
type phashEntry struct {
	artistID  string
	slotIndex int
	hash      uint64
}

// phashRow is one exists_flag=1 artist_images row as read across all artists.
type phashRow struct {
	artistID  string
	imageType string
	slotIndex int
	hashHex   string
}

// queryPHashRows loads every exists_flag=1 fanart and thumb row in the
// library. Deliberately unfiltered by artist even for a scoped scan: the
// registry needs the whole library (see PHashMismatchScope.ArtistID).
func queryPHashRows(ctx context.Context, db *sql.DB) ([]phashRow, int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT artist_id, image_type, slot_index, phash
		 FROM artist_images
		 WHERE exists_flag = 1 AND image_type IN ('fanart', 'thumb')
		 ORDER BY artist_id, image_type, slot_index`)
	if err != nil {
		return nil, 0, fmt.Errorf("querying image hashes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []phashRow
	scanErrors := 0
	for rows.Next() {
		var r phashRow
		if scanErr := rows.Scan(&r.artistID, &r.imageType, &r.slotIndex, &r.hashHex); scanErr != nil {
			scanErrors++
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, scanErrors, fmt.Errorf("iterating image hashes: %w", err)
	}
	return out, scanErrors, nil
}

// usablePHash parses a stored phash and reports whether it can be compared.
//
// Two values are rejected, for the same reason: an empty column is "never
// hashed", and a zero hash is indistinguishable from it. Zero is the trap that
// matters -- 0000000000000000 looks like data and is Hamming-distance 0 from
// every other zero, so admitting it would MANUFACTURE perfect collisions
// between every pair of unhashed images in the library. Unknown never matches
// unknown.
func usablePHash(hex string) (uint64, string, bool) {
	if hex == "" {
		return 0, "no stored perceptual hash", false
	}
	h, err := image.ParseHashHex(hex)
	if err != nil {
		return 0, "unparsable perceptual hash", false
	}
	if h == hashUnknown {
		return 0, "perceptual hash is the unknown sentinel", false
	}
	return h, "", true
}

// artistNames lists every artist id -> name, paging through the artist
// service. Needed both to name the other side of a collision and to iterate
// the scan in a stable order.
func (p *Pipeline) artistNames(ctx context.Context) (map[string]string, []string, error) {
	names := map[string]string{}
	var order []string
	page := 1
	for {
		artists, _, err := p.artistService.List(ctx, artist.ListParams{Page: page, PageSize: scanPHashPageSize})
		if err != nil {
			return nil, nil, fmt.Errorf("listing artists at page %d: %w", page, err)
		}
		if len(artists) == 0 {
			break
		}
		for i := range artists {
			names[artists[i].ID] = artists[i].Name
			order = append(order, artists[i].ID)
		}
		if len(artists) < scanPHashPageSize {
			break
		}
		page++
	}
	return names, order, nil
}

// phashIndex is the built comparison state for one scan.
type phashIndex struct {
	fanart         []phashEntry
	fanartByArtist map[string][]phashEntry
	thumbs         map[string]uint64
	fanartCoverage PHashRegistryCoverage
	thumbCoverage  PHashRegistryCoverage
	// unusableFanart records in-scope-checkable slots that carried no
	// usable hash, keyed by artist, with the reason.
	unusableFanart map[string][]PHashIndeterminateSlot
}

// buildPHashIndex turns raw rows into the fanart comparison registry, the
// thumb attribution registry, and the coverage figures for both.
func buildPHashIndex(rows []phashRow, names map[string]string) *phashIndex {
	idx := &phashIndex{
		fanartByArtist: map[string][]phashEntry{},
		thumbs:         map[string]uint64{},
		unusableFanart: map[string][]PHashIndeterminateSlot{},
	}
	// Track per-artist whether the artist contributed anything usable, so
	// "artists skipped" counts artists that dropped out of the registry
	// entirely rather than individual slots.
	//
	// Seeded from the WHOLE artist list, not from the rows, and that is the
	// load-bearing part. Seeding from rows only ever knows about artists that
	// HAVE an artist_images row, so an artist with no fanart row at all landed
	// in neither bucket and vanished from the coverage figures entirely: a
	// 1000-artist library where 998 have no fanart reported
	// "artists_indexed: 2, artists_skipped: 0", which reads as complete
	// coverage of a two-artist library. That is the exact false-green this
	// report exists to prevent -- an artist with no hash is invisible to every
	// other artist's scan whether the hash is missing (row present, unusable)
	// or the row is absent, so both must count as skipped. "Skipped" therefore
	// means "known artist that contributed no usable hash", which is the
	// honest reading. ArtistsIndexed is unchanged: it still counts only
	// artists that contributed at least one usable hash.
	fanartArtists := make(map[string]bool, len(names))
	thumbArtists := make(map[string]bool, len(names))
	for id := range names {
		fanartArtists[id] = false
		thumbArtists[id] = false
	}

	for _, r := range rows {
		h, reason, ok := usablePHash(r.hashHex)
		switch r.imageType {
		case "fanart":
			// An artist_images row whose artist is absent from names (a
			// row orphaned by a deleted artist) is still counted: it is
			// registry state either way, and dropping it here would
			// re-introduce the undercount from the other direction.
			if _, seen := fanartArtists[r.artistID]; !seen {
				fanartArtists[r.artistID] = false
			}
			if !ok {
				idx.fanartCoverage.SlotsSkipped++
				idx.unusableFanart[r.artistID] = append(idx.unusableFanart[r.artistID], PHashIndeterminateSlot{
					ArtistID:   r.artistID,
					ArtistName: names[r.artistID],
					SlotIndex:  r.slotIndex,
					Reason:     reason,
				})
				continue
			}
			e := phashEntry{artistID: r.artistID, slotIndex: r.slotIndex, hash: h}
			idx.fanart = append(idx.fanart, e)
			idx.fanartByArtist[r.artistID] = append(idx.fanartByArtist[r.artistID], e)
			idx.fanartCoverage.SlotsIndexed++
			fanartArtists[r.artistID] = true
		case "thumb":
			if _, seen := thumbArtists[r.artistID]; !seen {
				thumbArtists[r.artistID] = false
			}
			if !ok {
				idx.thumbCoverage.SlotsSkipped++
				continue
			}
			// One thumb per artist; a second row for the same artist is
			// not expected, and keeping the first is arbitrary but
			// stable given the ORDER BY.
			if _, dup := idx.thumbs[r.artistID]; !dup {
				idx.thumbs[r.artistID] = h
				idx.thumbCoverage.SlotsIndexed++
				thumbArtists[r.artistID] = true
			}
		}
	}

	for _, indexed := range fanartArtists {
		if indexed {
			idx.fanartCoverage.ArtistsIndexed++
		} else {
			idx.fanartCoverage.ArtistsSkipped++
		}
	}
	for _, indexed := range thumbArtists {
		if indexed {
			idx.thumbCoverage.ArtistsIndexed++
		} else {
			idx.thumbCoverage.ArtistsSkipped++
		}
	}
	return idx
}

// bestCrossArtistMatch finds the highest-similarity fanart slot belonging to a
// DIFFERENT artist, and how many other-artist slots matched at all.
func (idx *phashIndex) bestCrossArtistMatch(artistID string, hash uint64, tolerance float64) (phashEntry, float64, int) {
	var best phashEntry
	bestSim := 0.0
	count := 0
	for _, e := range idx.fanart {
		if e.artistID == artistID {
			continue
		}
		sim := image.Similarity(hash, e.hash)
		if sim < tolerance {
			continue
		}
		count++
		if sim > bestSim {
			best, bestSim = e, sim
		}
	}
	return best, bestSim, count
}

// thumbAttribution names an artist whose thumb this picture resembles. This is
// corroboration only: when it fires it identifies the true subject of the
// polluting image, and it fires rarely (a thumb and a backdrop are different
// photographs). It never raises a suspect by itself.
func (idx *phashIndex) thumbAttribution(artistID string, hash uint64, tolerance float64) (string, float64) {
	bestID := ""
	bestSim := 0.0
	for otherID, th := range idx.thumbs {
		if otherID == artistID {
			continue
		}
		sim := image.Similarity(hash, th)
		if sim >= tolerance && sim > bestSim {
			bestID, bestSim = otherID, sim
		}
	}
	return bestID, bestSim
}

// ScanPHashMismatches reports fanart slots that hold the same picture as a
// different artist's fanart slot -- the cross-artist backdrop pollution
// signal. Strictly read-only: it reads artist_images and compares stored
// hashes, touching neither the filesystem nor any platform, and is safe
// against a live instance.
//
// Slots with no usable hash are reported as indeterminate, never as clean.
// Populating them is the phash backfill's job (#2577), not this scan's: this
// path stays read-only.
func (p *Pipeline) ScanPHashMismatches(ctx context.Context, scope PHashMismatchScope) (PHashMismatchReport, error) {
	if p.engine == nil || p.engine.db == nil || p.artistService == nil {
		return PHashMismatchReport{}, fmt.Errorf("scan phash mismatches: pipeline not fully wired")
	}
	tolerance := scope.Tolerance
	// math.IsNaN is load-bearing here, not defensive noise. Every IEEE-754
	// comparison against NaN is false, so `tolerance <= 0 || tolerance > 1`
	// ADMITS NaN and the fallback never fires. NaN then defeats
	// bestCrossArtistMatch's `sim < tolerance` filter the same way -- nothing
	// is ever rejected, so EVERY cross-artist slot becomes a suspect. Measured:
	// four provably distinct pictures (Hamming 30-38 apart) produced four false
	// suspects at ~0.48 similarity. This method is public and PR-3's repair
	// path calls it directly, so a programmatic NaN would feed a
	// 100%-false-positive suspect set straight into a deletion.
	if math.IsNaN(tolerance) || tolerance <= 0 || tolerance > 1 {
		tolerance = defaultPHashMismatchTolerance
	}

	names, order, err := p.artistNames(ctx)
	if err != nil {
		return PHashMismatchReport{}, err
	}
	if scope.ArtistID != "" {
		if _, ok := names[scope.ArtistID]; !ok {
			return PHashMismatchReport{}, fmt.Errorf("scan phash mismatches: artist %s not found", scope.ArtistID)
		}
	}

	rows, scanErrors, err := queryPHashRows(ctx, p.engine.db)
	if err != nil {
		return PHashMismatchReport{}, err
	}
	idx := buildPHashIndex(rows, names)

	report := PHashMismatchReport{
		Tolerance:      tolerance,
		ScopedArtistID: scope.ArtistID,
		FanartRegistry: idx.fanartCoverage,
		ThumbRegistry:  idx.thumbCoverage,
		ScanErrors:     scanErrors,
	}
	if idx.fanartCoverage.ArtistsSkipped > 0 || idx.fanartCoverage.SlotsSkipped > 0 {
		// Loud on purpose. Every skipped slot is a picture that cannot be
		// recognized as pollution in ANY artist's folder, so the gap
		// suppresses findings library-wide rather than only for its owner.
		p.logger.Warn("phash mismatch registry is incomplete; findings are under-reported library-wide",
			slog.Int("artists_indexed", idx.fanartCoverage.ArtistsIndexed),
			slog.Int("artists_skipped", idx.fanartCoverage.ArtistsSkipped),
			slog.Int("slots_indexed", idx.fanartCoverage.SlotsIndexed),
			slog.Int("slots_skipped", idx.fanartCoverage.SlotsSkipped))
	}

	for _, artistID := range order {
		// Also checked here, not only inside suspectsForArtist: an artist
		// with no hashed fanart slots never enters the inner loop, so a
		// library where most artists have no fanart would otherwise run to
		// completion after cancellation without ever testing ctx.
		if err := ctx.Err(); err != nil {
			return PHashMismatchReport{}, fmt.Errorf("scan phash mismatches: %w", err)
		}
		if scope.ArtistID != "" && artistID != scope.ArtistID {
			continue
		}
		report.ArtistsScanned++
		report.Indeterminate = append(report.Indeterminate, idx.unusableFanart[artistID]...)

		suspects, err := p.suspectsForArtist(ctx, idx, artistID, names, tolerance, &report)
		if err != nil {
			// The half-built report is DISCARDED, not returned. An
			// abandoned scan that surfaced its partial suspect list would
			// be indistinguishable from a completed one that found less,
			// and this detector feeds a deletion path: a short suspect
			// list read as complete is a false-green, and a short
			// "0 suspects" read as clean is the same bug wearing the
			// other hat. Callers get an error and no report.
			return PHashMismatchReport{}, fmt.Errorf("scan phash mismatches: %w", err)
		}
		if len(suspects) == 0 {
			continue
		}
		report.ArtistsAffected++
		report.PerArtist = append(report.PerArtist, ArtistPHashMismatch{
			ArtistID: artistID, Name: names[artistID], Suspects: suspects,
		})
	}
	report.IndeterminateSlots = len(report.Indeterminate)
	sort.Slice(report.PerArtist, func(i, j int) bool { return report.PerArtist[i].Name < report.PerArtist[j].Name })

	p.logger.Info("phash mismatch scan complete",
		slog.String("scoped_artist_id", scope.ArtistID),
		slog.Float64("tolerance", tolerance),
		slog.Int("artists_scanned", report.ArtistsScanned),
		slog.Int("artists_affected", report.ArtistsAffected),
		slog.Int("slots_evaluated", report.SlotsEvaluated),
		slog.Int("suspect_slots", report.SuspectSlots),
		slog.Int("indeterminate_slots", report.IndeterminateSlots),
		slog.Int("scan_errors", report.ScanErrors))
	return report, nil
}

// suspectsForArtist compares one artist's hashed fanart slots against the
// whole-library registry, tallying evaluated and suspect slots into report.
//
// The comparison is all-pairs and CPU-bound with no I/O in it, so ctx is
// checked here -- once per SLOT rather than once per comparison. The maintainer's
// budget for this scan is ~1k hashes, i.e. ~500k iterations of a 64-bit XOR plus
// popcount, and a ctx.Err() per iteration would cost more than the comparison it
// guards. Per-slot keeps the check count at the slot count (~1k) while bounding
// cancellation latency to one pass over the registry, which is the same ~1k
// XOR+popcounts -- microseconds. Without this the CPU loop is uninterruptible and
// a disconnected API client leaves the scan burning a core to completion.
func (p *Pipeline) suspectsForArtist(ctx context.Context, idx *phashIndex, artistID string, names map[string]string, tolerance float64, report *PHashMismatchReport) ([]PHashCollision, error) {
	var suspects []PHashCollision
	for _, slot := range idx.fanartByArtist[artistID] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report.SlotsEvaluated++
		match, sim, count := idx.bestCrossArtistMatch(artistID, slot.hash, tolerance)
		if count == 0 {
			continue
		}
		report.SuspectSlots++
		c := PHashCollision{
			SlotIndex:         slot.slotIndex,
			PHash:             image.HashHex(slot.hash),
			MatchedArtistID:   match.artistID,
			MatchedArtistName: names[match.artistID],
			MatchedSlotIndex:  match.slotIndex,
			Similarity:        sim,
			MatchCount:        count,
		}
		if attrID, attrSim := idx.thumbAttribution(artistID, slot.hash, tolerance); attrID != "" {
			c.ThumbAttributionArtistID = attrID
			c.ThumbAttributionArtistName = names[attrID]
			p.logger.Info("phash mismatch corroborated by thumb attribution",
				slog.String("artist_id", artistID), slog.Int("slot_index", slot.slotIndex),
				slog.String("attributed_artist_id", attrID), slog.Float64("thumb_similarity", attrSim))
		}
		p.logger.Info("phash mismatch suspect",
			slog.String("artist_id", artistID), slog.String("artist", names[artistID]),
			slog.Int("slot_index", slot.slotIndex), slog.String("phash", c.PHash),
			slog.String("matched_artist_id", match.artistID), slog.Int("matched_slot_index", match.slotIndex),
			slog.Float64("similarity", sim), slog.Int("match_count", count))
		suspects = append(suspects, c)
	}
	return suspects, nil
}
