package rule

import (
	"context"
	"fmt"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// Skip reasons. These are operator-facing strings: they appear in the health
// API response and in the evaluation log line, so they say what is missing in
// plain language rather than naming code.
const (
	// SkipReasonNoLocalPath is the long-standing FilesystemDependent skip:
	// the rule needs files on disk and the artist has none (an API-only
	// import from Emby or Jellyfin).
	SkipReasonNoLocalPath = "artist has no local filesystem path"
	// SkipReasonNoDatabase means the engine was built without a database
	// handle, so a rule that can only read stored data cannot run at all.
	SkipReasonNoDatabase = "rule engine has no database handle"
	// SkipReasonNoComparablePerceptualHashes means the artist has no local
	// files to hash and DOES have images that could hold a duplicate (two or
	// more), but fewer than two of them carry a usable perceptual hash -- so
	// the comparison is genuinely blind.
	//
	// It is NOT the reason for an artist with fewer than two images: no pair
	// exists there, so the rule is trivially satisfied and PASSES. Reporting
	// "could not evaluate" for that artist would be the mirror image of #2509:
	// claiming an ignorance the code does not have, where the old code claimed
	// a pass it had not earned.
	SkipReasonNoComparablePerceptualHashes = "artist has no local files and, of its 2 or more images, fewer than 2 carry a stored perceptual hash to compare"
	// SkipReasonNoComparableContentHashes is the exact-duplicate equivalent,
	// counted over FANART rows only (the exact rule groups fanart by content
	// hash and looks at nothing else): two or more fanart images, fewer than
	// two of them with a stored content hash.
	SkipReasonNoComparableContentHashes = "artist has no local files and, of its 2 or more fanart images, fewer than 2 carry a stored content hash to compare"
)

// SkippedRule records a rule that was NOT evaluated for an artist, and why.
//
// This is deliberately decided BEFORE any checker runs (see eligibleRules), not
// reported back by a checker afterwards. A checker-reported "I could not run"
// would be invisible to EligibleRuleIDs, which is the denominator of the offline
// health recompute; the rule would stay in that denominator with no result row,
// count as "missing" on every pass, and freeze the artist's health score forever
// (see Pipeline.offlineHealthScore). Deciding eligibility up front keeps the
// evaluator and the recompute deriving from exactly one function.
type SkippedRule struct {
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Reason   string `json:"reason"`
}

// ruleCapability reports whether a rule can meaningfully be evaluated against
// this specific artist, given the data actually available for that artist.
//
// It is the general seam for per-(rule, artist) eligibility. FilesystemDependent
// is the degenerate special case of it that predates it (needs a path, full
// stop); a capability can additionally consult the database. Follow-on work that
// gates a rule on, say, a configured metadata provider (#2476) registers another
// capability here rather than forking a second mechanism.
//
// ok=false MUST come with a non-empty reason: an unexplained skip is how a rule
// silently stops being enforced.
type ruleCapability func(ctx context.Context, a *artist.Artist) (ok bool, reason string, err error)

// imageHashCapability summarizes, for one artist, both how many images each
// duplicate rule would CONSIDER and how many of them it can actually COMPARE.
//
// Both halves are needed, because "cannot compare" and "nothing to compare" are
// different answers. An artist with fewer than two candidate images for a rule
// has no pair at all, so no duplicate can exist and the rule is trivially
// satisfied: that is a genuine PASS, not an inability. Only an artist that HAS a
// pair of candidates but cannot compare them (fewer than two usable hashes among
// them) is genuinely blind, and only that artist is skipped.
//
// The two rules compare DIFFERENT columns over DIFFERENT candidate sets and are
// not interchangeable:
//
//   - image_duplicate compares phash across ALL image types (pairImageDuplicates
//     walks every member pair regardless of type), and deliberately excludes an
//     unknown (zero) phash, since Similarity(0, 0) is 1.0 and would report two
//     unhashed images as identical. Candidates: every exists_flag=1 row.
//     Comparable: those with a non-zero phash.
//   - image_duplicate_exact groups FANART rows by content_hash and ignores every
//     other type outright (see exactFanartDuplicates; non-fanart types are
//     single-slot and cannot hold a within-type duplicate). Candidates: fanart
//     rows. Comparable: fanart rows with a known content hash.
//
// An artist can therefore have enough data for one rule and not the other, so
// each rule's counts are tracked separately.
type imageHashCapability struct {
	// perceptualCandidates counts every exists_flag=1 row (any image type):
	// the set pairImageDuplicates draws its pairs from.
	perceptualCandidates int
	// perceptualComparable counts rows (any image type) with a usable,
	// non-zero perceptual hash.
	perceptualComparable int
	// exactFanartCandidates counts fanart rows, hashed or not: the only rows
	// exactFanartDuplicates looks at.
	exactFanartCandidates int
	// exactFanartComparable counts fanart rows with a known content hash.
	exactFanartComparable int
}

// imageHashCapabilities loads the per-artist image-hash summary, computing it at
// most once per evaluation.
//
// Cost: at most ONE query per artist per EvaluateScoped CALL, not one per
// evaluation pass. The cache is keyed by artist ID and cleared at the top of
// EvaluateScoped, exactly like imageDupCache and sharedFSCache: fresh per call,
// shared within one. Within a call it collapses the four consultations that would
// otherwise happen -- both duplicate rules' capability checks, plus the
// EligibleRuleIDs the offline health recompute makes right afterwards off the same
// still-warm cache -- into one query.
//
// A run that calls EvaluateScoped more than once for the same artist pays once per
// call: the category path (runForArtistFiltered) queries three times -- once for
// ScopeForCategory, once for the scoped evaluation, once for the post-fix
// re-evaluation -- because each of the latter two clears the cache on entry. That
// is deliberate and it is the correct trade: a re-read is how a run that just
// deleted a duplicate image sees the new image set rather than the one it started
// with. The query is a single indexed lookup on artist_images(artist_id); the cache
// exists to stop the fan-out WITHIN a call, not to memoize across the run.
//
// A clear racing another artist's in-flight populate costs that artist one
// redundant query, never wrong data.
//
// Artists WITH a local path never reach here (the capability short-circuits on
// a.Path != ""), so the query is confined to API-only artists.
func (e *Engine) imageHashCapabilities(ctx context.Context, a *artist.Artist) (imageHashCapability, error) {
	e.imageCapMu.Lock()
	if cached, ok := e.imageCapCache[a.ID]; ok {
		e.imageCapMu.Unlock()
		return cached, nil
	}
	e.imageCapMu.Unlock()

	rows, err := e.db.QueryContext(ctx,
		`SELECT image_type, phash, content_hash FROM artist_images WHERE artist_id = ? AND exists_flag = 1`,
		a.ID)
	if err != nil {
		return imageHashCapability{}, fmt.Errorf("querying image hashes for rule eligibility: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var capa imageHashCapability
	for rows.Next() {
		var imageType, phashHex, contentHash string
		if scanErr := rows.Scan(&imageType, &phashHex, &contentHash); scanErr != nil {
			e.logger.Debug("scanning image hash row for rule eligibility",
				"artist", a.Name, "error", scanErr)
			continue
		}
		capa.perceptualCandidates++
		if h, parseErr := image.ParseHashHex(phashHex); parseErr == nil && h != hashUnknown {
			capa.perceptualComparable++
		}
		if imageType == "fanart" {
			capa.exactFanartCandidates++
			if contentHash != contentHashUnknown {
				capa.exactFanartComparable++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return imageHashCapability{}, fmt.Errorf("iterating image hashes for rule eligibility: %w", err)
	}

	e.imageCapMu.Lock()
	if e.imageCapCache == nil {
		e.imageCapCache = make(map[string]imageHashCapability)
	}
	e.imageCapCache[a.ID] = capa
	e.imageCapMu.Unlock()

	return capa, nil
}

// capImageDuplicate is the capability predicate for the perceptual duplicate
// rule. The rule is eligible when ANY of these holds:
//
//   - the artist has a local directory -- files can be read and hashed on demand;
//   - the artist has fewer than TWO images at all -- pairImageDuplicates draws its
//     pairs from every exists_flag=1 row, so with fewer than two rows there is no
//     pair, no duplicate is possible, and the rule is trivially satisfied. This is
//     a genuine PASS, and on a real library it is the dominant case among path-less
//     artists. It is also the steady state after every successful de-duplication:
//     fix a path-less artist down to one image and the rule must report a clean
//     pass, not stop reporting on it;
//   - at least two of its images carry a usable perceptual hash -- enough data to
//     compare without touching a filesystem.
//
// It is skipped ONLY when a pair of candidate images exists but fewer than two of
// them can be compared: that, and only that, is a blind comparison.
func (e *Engine) capImageDuplicate(ctx context.Context, a *artist.Artist) (bool, string, error) {
	if e.db == nil {
		return false, SkipReasonNoDatabase, nil
	}
	if a.Path != "" {
		return true, "", nil
	}
	capa, err := e.imageHashCapabilities(ctx, a)
	if err != nil {
		return false, "", err
	}
	if capa.perceptualCandidates < 2 || capa.perceptualComparable >= 2 {
		return true, "", nil
	}
	return false, SkipReasonNoComparablePerceptualHashes, nil
}

// capImageDuplicateExact is the capability predicate for the byte-identical
// duplicate rule. Same shape as capImageDuplicate, but every count is taken over
// FANART rows and CONTENT hashes: exactFanartDuplicates groups fanart by
// content_hash and never looks at another image type or at a perceptual hash, so
// an artist with one fanart image (and any number of thumbs, logos or banners)
// cannot hold an exact duplicate and trivially PASSES -- it is not skipped.
func (e *Engine) capImageDuplicateExact(ctx context.Context, a *artist.Artist) (bool, string, error) {
	if e.db == nil {
		return false, SkipReasonNoDatabase, nil
	}
	if a.Path != "" {
		return true, "", nil
	}
	capa, err := e.imageHashCapabilities(ctx, a)
	if err != nil {
		return false, "", err
	}
	if capa.exactFanartCandidates < 2 || capa.exactFanartComparable >= 2 {
		return true, "", nil
	}
	return false, SkipReasonNoComparableContentHashes, nil
}
