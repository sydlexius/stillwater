package rule

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// Regression suite for #2509: the two image-duplicate rules opened with
//
//	if a.Path == "" || e.db == nil { return nil }
//
// and a nil return from a Checker means "no violation", i.e. PASSED. So every
// API-only artist (no directory on disk) was recorded as PASSING both duplicate
// rules and had those passes counted in its health score, without either rule
// ever looking at it. On the maintainer's library that was 157 of 865 artists.
//
// The fix has two halves and both are pinned here:
//
//  1. A rule that CANNOT apply to an artist is now SKIPPED before any checker
//     runs (eligibleRules -> ruleCapability), so it is out of the denominator
//     entirely rather than being scored as a pass.
//  2. A rule that CAN apply -- a path-less artist with enough stored hashes to
//     compare -- is now genuinely EVALUATED from artist_images and can raise a
//     violation.
//
// Read the doc comment on each test before weakening it: each names the mutant
// it exists to kill.

// oneBitApart returns a hash 1 bit away from h. dHash Similarity is
// 1 - hamming/64, so a 1-bit difference is 63/64 = 0.984, comfortably above the
// 0.90 default tolerance and therefore a genuine perceptual duplicate.
func oneBitApart(h uint64) uint64 { return h ^ 1 }

// insertAPIImage seeds one artist_images row exactly as an API-only import
// leaves it: exists_flag set, hashes recorded by the image pipeline, and no file
// anywhere on disk. phash of 0 / content_hash of "" mean "not hashed" and are
// what the capability gate treats as not comparable.
func insertAPIImage(t *testing.T, db *sql.DB, artistID, imageType string, slot int, phash uint64, contentHash string) {
	t.Helper()
	phashHex := ""
	if phash != hashUnknown {
		phashHex = image.HashHex(phash)
	}
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		fmt.Sprintf("%s-%s-%d", artistID, imageType, slot), artistID, imageType, slot, phashHex, contentHash)
	if err != nil {
		t.Fatalf("inserting artist_images row: %v", err)
	}
}

// insertBlindFanartPair seeds the ONLY shape in which a duplicate rule is
// genuinely SKIPPED for a path-less artist: TWO fanart images -- so a pair
// exists and a duplicate is possible -- with neither a perceptual nor a content
// hash on either, so nothing about that pair can be compared.
//
// One unhashed image is NOT this case. With fewer than two candidates no pair
// exists, no duplicate is possible, and both rules PASS. Seeding a single image
// and expecting a skip is what the old (wrong) predicate did.
func insertBlindFanartPair(t *testing.T, db *sql.DB, artistID string) {
	t.Helper()
	insertAPIImage(t, db, artistID, "fanart", 0, hashUnknown, "")
	insertAPIImage(t, db, artistID, "fanart", 1, hashUnknown, "")
}

// apiOnlyArtist creates an artist with NO filesystem path: the Emby/Jellyfin
// import case this whole issue is about.
func apiOnlyArtist(t *testing.T, db *sql.DB, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{Name: name, Path: ""}
	if err := artist.NewService(db).Create(t.Context(), a); err != nil {
		t.Fatalf("creating API-only artist: %v", err)
	}
	if a.Path != "" {
		t.Fatalf("precondition: artist must have no path, got %q", a.Path)
	}
	return a
}

// dupRuleEngine seeds the default rules, enables image_duplicate (it ships
// disabled, and a disabled rule is never eligible, so without this every
// assertion about it below would be vacuous), and returns a real DB-backed
// engine.
func dupRuleEngine(t *testing.T, db *sql.DB) (*Engine, *Service) {
	t.Helper()
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	r, err := svc.GetByID(ctx, RuleImageDuplicate)
	if err != nil {
		t.Fatalf("loading %s: %v", RuleImageDuplicate, err)
	}
	r.Enabled = true
	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("enabling %s: %v", RuleImageDuplicate, err)
	}
	// PRECONDITION: both rules must be enabled, or "was it skipped?" and "did it
	// find a duplicate?" are both unanswerable and every test below passes for
	// the wrong reason.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		got, err := svc.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("loading %s: %v", id, err)
		}
		if !got.Enabled {
			t.Fatalf("precondition: rule %s must be enabled for this test to mean anything", id)
		}
	}
	return NewEngine(svc, db, nil, nil, testLogger()), svc
}

func skipReasonFor(res *EvaluationResult, ruleID string) (string, bool) {
	for _, s := range res.RulesSkipped {
		if s.RuleID == ruleID {
			return s.Reason, true
		}
	}
	return "", false
}

func violationFor(res *EvaluationResult, ruleID string) (*Violation, bool) {
	for i := range res.Violations {
		if res.Violations[i].RuleID == ruleID {
			return &res.Violations[i], true
		}
	}
	return nil, false
}

// TestPathlessArtist_PerceptualDuplicateIsDetected is the acceptance criterion.
// A path-less artist with two near-identical STORED perceptual hashes has a real
// duplicate, and the rule must say so.
//
// Mutant this kills: restoring `if a.Path == ""` in makeImageDuplicateChecker or
// findImageDuplicatesImpl. Either one turns this artist back into a silent pass.
func TestPathlessArtist_PerceptualDuplicateIsDetected(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Perceptual Dup")

	const base uint64 = 0x0F1E2D3C4B5A6978
	insertAPIImage(t, db, a.ID, "fanart", 0, base, "")
	insertAPIImage(t, db, a.ID, "fanart", 1, oneBitApart(base), "")

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// PRECONDITION: the rule must actually have RUN. If it were skipped, the
	// "violation raised" assertion would fail for a reason that has nothing to do
	// with detection, and a future edit could "fix" it by making the rule
	// ineligible again -- which is the bug.
	if !slices.Contains(res.RulesConsidered, RuleImageDuplicate) {
		t.Fatalf("precondition: %s was not evaluated for a path-less artist with two "+
			"comparable stored phashes; considered=%v skipped=%v",
			RuleImageDuplicate, res.RulesConsidered, res.RulesSkipped)
	}

	v, ok := violationFor(res, RuleImageDuplicate)
	if !ok {
		t.Fatalf("no %s violation for a path-less artist whose two stored fanart phashes "+
			"are 1 bit apart (98%% similar). The rule was recorded as PASSING an artist it "+
			"never examined -- this is #2509.", RuleImageDuplicate)
	}
	if v.Fixable {
		t.Error("violation is marked Fixable, but the fixer deletes and renumbers FILES " +
			"and this artist has no directory; a fix attempt could only no-op")
	}
}

// TestPathlessArtist_CrossTypeDuplicateIsDetected pins that the perceptual rule
// compares across image TYPES, not just fanart slots, for a path-less artist: a
// thumb and a fanart holding the same picture is a real finding. It is reported
// but not fixable -- resolving it means replacing one image with a distinct
// alternative, which no fixer can choose (and there are no files here anyway).
func TestPathlessArtist_CrossTypeDuplicateIsDetected(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Cross Type")

	const base uint64 = 0x123456789ABCDEF0
	insertAPIImage(t, db, a.ID, "thumb", 0, base, "")
	insertAPIImage(t, db, a.ID, "fanart", 0, oneBitApart(base), "")

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !slices.Contains(res.RulesConsidered, RuleImageDuplicate) {
		t.Fatalf("precondition: %s was not evaluated; the capability gate must count usable "+
			"phashes across ALL image types, not fanart alone. skipped=%v",
			RuleImageDuplicate, res.RulesSkipped)
	}
	v, ok := violationFor(res, RuleImageDuplicate)
	if !ok {
		t.Fatalf("no %s violation for a path-less artist whose thumb and fanart are 98%% similar",
			RuleImageDuplicate)
	}
	if v.Fixable {
		t.Error("a cross-type duplicate is not fixable: resolving it requires choosing a " +
			"distinct replacement image")
	}
}

// TestPathlessArtist_DistinctHashesPassHonestly is the other side of the AC: the
// rule runs and legitimately finds nothing. Without this, making every path-less
// artist violate would also satisfy the test above.
func TestPathlessArtist_DistinctHashesPassHonestly(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Distinct")

	// Bitwise complements: hamming distance 64, similarity 0.0.
	insertAPIImage(t, db, a.ID, "fanart", 0, 0xAAAAAAAAAAAAAAAA, "")
	insertAPIImage(t, db, a.ID, "fanart", 1, 0x5555555555555555, "")

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !slices.Contains(res.RulesConsidered, RuleImageDuplicate) {
		t.Fatalf("precondition: %s must have been EVALUATED (two comparable phashes exist); "+
			"a 'pass' that came from a skip proves nothing. skipped=%v",
			RuleImageDuplicate, res.RulesSkipped)
	}
	if v, ok := violationFor(res, RuleImageDuplicate); ok {
		t.Errorf("false positive: two maximally dissimilar hashes reported as duplicates: %s", v.Message)
	}
}

// TestPathlessArtist_ExactDuplicateIsDetected pins the CONTENT-hash tier, and
// with it the phash-vs-content_hash asymmetry: these rows carry byte-identical
// content hashes and NO perceptual hash at all. The exact rule must find the
// duplicate; the perceptual rule must be SKIPPED, not passed, because it has
// nothing it can compare.
//
// Mutant this kills: collapsing the two capability predicates into one (e.g.
// gating both on the perceptual-hash count). That would mark the exact rule
// ineligible here and lose a real, byte-certain duplicate.
func TestPathlessArtist_ExactDuplicateIsDetected(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Exact Dup")

	const sameBytes = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	insertAPIImage(t, db, a.ID, "fanart", 0, hashUnknown, sameBytes)
	insertAPIImage(t, db, a.ID, "fanart", 1, hashUnknown, sameBytes)

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if !slices.Contains(res.RulesConsidered, RuleImageDuplicateExact) {
		t.Fatalf("precondition: %s must have been evaluated; considered=%v skipped=%v",
			RuleImageDuplicateExact, res.RulesConsidered, res.RulesSkipped)
	}
	v, ok := violationFor(res, RuleImageDuplicateExact)
	if !ok {
		t.Fatalf("no %s violation for a path-less artist with two byte-identical stored "+
			"content hashes", RuleImageDuplicateExact)
	}
	if v.Fixable {
		t.Error("Fixable is true, but there are no files to delete or renumber")
	}

	// The asymmetry: no phash anywhere, so the perceptual rule cannot compare
	// anything and must be SKIPPED rather than passed.
	if slices.Contains(res.RulesConsidered, RuleImageDuplicate) {
		t.Errorf("%s was evaluated with zero usable perceptual hashes; it can only have "+
			"'passed' vacuously", RuleImageDuplicate)
	}
	reason, skipped := skipReasonFor(res, RuleImageDuplicate)
	if !skipped {
		t.Fatalf("%s is neither considered nor skipped: it has vanished silently. skipped=%v",
			RuleImageDuplicate, res.RulesSkipped)
	}
	if reason != SkipReasonNoComparablePerceptualHashes {
		t.Errorf("skip reason = %q; want %q", reason, SkipReasonNoComparablePerceptualHashes)
	}
}

// TestPathlessArtist_PerceptualOnlyHashesSkipTheExactRule is the converse
// asymmetry: perceptual hashes present, content hashes absent. The perceptual
// rule runs; the exact rule has no bytes to compare and is skipped.
func TestPathlessArtist_PerceptualOnlyHashesSkipTheExactRule(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Perceptual Only")

	insertAPIImage(t, db, a.ID, "fanart", 0, 0xAAAAAAAAAAAAAAAA, "")
	insertAPIImage(t, db, a.ID, "fanart", 1, 0x5555555555555555, "")

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !slices.Contains(res.RulesConsidered, RuleImageDuplicate) {
		t.Errorf("%s should be eligible (2 stored phashes); skipped=%v", RuleImageDuplicate, res.RulesSkipped)
	}
	if slices.Contains(res.RulesConsidered, RuleImageDuplicateExact) {
		t.Errorf("%s was evaluated with zero stored content hashes; it can only pass vacuously",
			RuleImageDuplicateExact)
	}
	reason, skipped := skipReasonFor(res, RuleImageDuplicateExact)
	if !skipped {
		t.Fatalf("%s neither considered nor skipped", RuleImageDuplicateExact)
	}
	if reason != SkipReasonNoComparableContentHashes {
		t.Errorf("skip reason = %q; want %q", reason, SkipReasonNoComparableContentHashes)
	}
}

// TestPathlessArtist_NothingToCompareIsAPassNotASkip is the boundary this fix
// moved, and the DOMINANT case on a real library: most path-less artists have
// fewer than two images.
//
// An artist with 0 or 1 candidate images for a rule CAN be evaluated. No pair
// exists, so no duplicate can exist, so the rule is trivially satisfied and it
// PASSES. Reporting "could not evaluate" there claims an ignorance the code does
// not have -- the mirror image of #2509's claimed-pass-it-never-earned. It is
// also the steady state after every successful de-duplication: de-dupe a
// path-less artist that had two images down to one and the rule must report a
// clean pass, not stop reporting on it.
//
// Mutant this kills: gating eligibility on the comparable-hash count ALONE (the
// predicate as first written), which reports every one of these artists as
// SKIPPED.
func TestPathlessArtist_NothingToCompareIsAPassNotASkip(t *testing.T) {
	tests := []struct {
		name  string
		seed  func(t *testing.T, db *sql.DB, artistID string)
		rules []string
	}{
		{
			name:  "no images at all",
			seed:  func(*testing.T, *sql.DB, string) {},
			rules: []string{RuleImageDuplicate, RuleImageDuplicateExact},
		},
		{
			name: "one unhashed image",
			seed: func(t *testing.T, db *sql.DB, id string) {
				insertAPIImage(t, db, id, "fanart", 0, hashUnknown, "")
			},
			rules: []string{RuleImageDuplicate, RuleImageDuplicateExact},
		},
		{
			name: "one hashed image",
			seed: func(t *testing.T, db *sql.DB, id string) {
				insertAPIImage(t, db, id, "fanart", 0, 0xAAAAAAAAAAAAAAAA, "abc123")
			},
			rules: []string{RuleImageDuplicate, RuleImageDuplicateExact},
		},
		{
			// The exact rule looks at FANART rows only (exactFanartDuplicates
			// ignores every other type), so one fanart plus any number of
			// single-slot images is still "no pair" for it -- even though the
			// perceptual rule, which compares across types, has two candidates
			// here and cannot compare them, so IT is skipped. The two rules are
			// asymmetric and must not be collapsed.
			name: "one fanart plus an unhashed thumb: exact still has no pair",
			seed: func(t *testing.T, db *sql.DB, id string) {
				insertAPIImage(t, db, id, "fanart", 0, hashUnknown, "")
				insertAPIImage(t, db, id, "thumb", 0, hashUnknown, "")
			},
			rules: []string{RuleImageDuplicateExact},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := t.Context()
			engine, _ := dupRuleEngine(t, db)
			a := apiOnlyArtist(t, db, "API "+tc.name)
			tc.seed(t, db, a.ID)

			res, err := engine.Evaluate(ctx, a)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			eligible, err := engine.EligibleRuleIDs(ctx, a)
			if err != nil {
				t.Fatalf("EligibleRuleIDs: %v", err)
			}

			for _, id := range tc.rules {
				if reason, skipped := skipReasonFor(res, id); skipped {
					t.Errorf("%s was SKIPPED (%q) for an artist with fewer than 2 candidate "+
						"images. There is no pair, so no duplicate is possible and the rule is "+
						"trivially satisfied: this is a genuine PASS. Reporting it as "+
						"'could not evaluate' claims an ignorance the code does not have, and "+
						"hides the rule from the majority of path-less artists on a real "+
						"library.", id, reason)
				}
				if !slices.Contains(res.RulesConsidered, id) {
					t.Errorf("%s is neither considered nor skipped for this artist: it has "+
						"vanished from the evaluation entirely", id)
				}
				if !slices.Contains(eligible, id) {
					t.Errorf("%s is missing from EligibleRuleIDs, so it is out of the health "+
						"DENOMINATOR for an artist it genuinely passes", id)
				}
				if v, ok := violationFor(res, id); ok {
					t.Errorf("%s raised a violation (%s) for an artist that has no pair of "+
						"images to compare; a duplicate is impossible here", id, v.Message)
				}
			}
		})
	}
}

// TestPathlessArtist_NoUsableHashesSkipsBothRules is the core of the fix: an
// artist the duplicate rules cannot speak to is SKIPPED, not passed. It must be
// absent from RulesTotal, RulesConsidered and EligibleRuleIDs, and it must not
// be counted in RulesPassed.
//
// "Cannot speak to" means exactly one thing: two or more candidate images (so a
// duplicate IS possible) and fewer than two comparable hashes among them (so the
// comparison would be blind). On a real library that is a small handful of the
// path-less artists, not the majority.
//
// Mutant this kills: dropping the capability check from eligibleRules. The rules
// would then run, find nothing to compare, return nil, and be scored as two
// passes -- the exact defect #2509 reports.
func TestPathlessArtist_NoUsableHashesSkipsBothRules(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	ctx := t.Context()
	a := apiOnlyArtist(t, db, "API No Hashes")

	// TWO images, neither hashed: a pair exists, so a duplicate is possible, but
	// nothing about it can be compared. This is the only genuinely blind case.
	insertBlindFanartPair(t, db, a.ID)

	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// POSITIVE CONTROL: the evaluation genuinely ran rules against this artist.
	// Without this, "the duplicate rules are absent" would also hold if the whole
	// evaluation had silently done nothing.
	if res.RulesTotal == 0 {
		t.Fatal("positive control FAILED: no rules ran at all, so every assertion below is vacuous")
	}

	eligible, err := engine.EligibleRuleIDs(ctx, a)
	if err != nil {
		t.Fatalf("EligibleRuleIDs: %v", err)
	}

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if slices.Contains(res.RulesConsidered, id) {
			t.Errorf("%s was EVALUATED for an artist with no comparable hashes. It cannot have "+
				"examined anything, so its 'pass' is fictional and inflates the health score (#2509).", id)
		}
		if slices.Contains(eligible, id) {
			t.Errorf("%s is still in EligibleRuleIDs, so it stays in the health DENOMINATOR "+
				"with no result. That is what freezes the artist's score forever.", id)
		}
		reason, skipped := skipReasonFor(res, id)
		if !skipped {
			t.Errorf("%s is not reported as skipped: the operator has no way to tell it was "+
				"never evaluated. skipped=%v", id, res.RulesSkipped)
			continue
		}
		if reason == "" {
			t.Errorf("%s skipped with an empty reason; an unexplained skip is how a rule "+
				"quietly stops being enforced", id)
		}
	}

	// RulesPassed + len(Violations) must equal RulesTotal, and RulesTotal must
	// exclude the skipped rules: a skip is neither a pass nor a failure.
	if res.RulesPassed+len(res.Violations) != res.RulesTotal {
		t.Errorf("passed(%d) + violations(%d) != total(%d)",
			res.RulesPassed, len(res.Violations), res.RulesTotal)
	}
	if res.RulesTotal != len(res.RulesConsidered) {
		t.Errorf("RulesTotal(%d) != len(RulesConsidered)=%d", res.RulesTotal, len(res.RulesConsidered))
	}
}

// TestPathlessArtist_FilesystemDependentSkipIsReported covers the pre-existing
// FilesystemDependent skip, which was correct but completely silent: nothing told
// the operator that nfo_exists was never evaluated for an API artist. It now
// appears in the skipped set with a reason, through the same mechanism.
func TestPathlessArtist_FilesystemDependentSkipIsReported(t *testing.T) {
	db := setupTestDB(t)
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API NFO Skip")

	res, err := engine.Evaluate(t.Context(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if slices.Contains(res.RulesConsidered, RuleNFOExists) {
		t.Fatalf("precondition: %s must not be evaluated for a path-less artist", RuleNFOExists)
	}
	reason, skipped := skipReasonFor(res, RuleNFOExists)
	if !skipped {
		t.Fatalf("%s was skipped silently: it is absent from both RulesConsidered and "+
			"RulesSkipped, so nothing tells the operator it was never checked. skipped=%v",
			RuleNFOExists, res.RulesSkipped)
	}
	if reason != SkipReasonNoLocalPath {
		t.Errorf("skip reason = %q; want %q", reason, SkipReasonNoLocalPath)
	}
}

// TestOfflineHealthScore_SkippedRulesDoNotBlockScoring is the most important
// regression guard, and the reason the fix is an ELIGIBILITY gate rather than a
// marker returned by the checker.
//
// offlineHealthScore takes its denominator from EligibleRuleIDs and REFUSES to
// score if any eligible rule has neither a fresh result nor a persisted
// rule_results row. A design where the checker reports "I could not evaluate
// this" after the fact would leave the rule eligible with no row, so it would
// land in `missing` on every single pass and the artist's health score would
// never update again -- permanently, for every API-only artist.
//
// This test pins the invariant that makes that impossible: every eligible rule
// has a persisted row after a full run, no skipped rule has one, and the score
// is produced rather than refused.
func TestOfflineHealthScore_SkippedRulesDoNotBlockScoring(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	artistSvc := artist.NewService(db)
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Health")
	// Bucket C: two images, no comparable hashes -- the only shape in which the
	// duplicate rules are genuinely skipped, which is what this test needs.
	insertBlindFanartPair(t, db, a.ID)

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	eligible, err := engine.EligibleRuleIDs(ctx, a)
	if err != nil {
		t.Fatalf("EligibleRuleIDs: %v", err)
	}
	// POSITIVE CONTROL: some rules ARE eligible for this artist. If none were,
	// "no eligible rule is missing a row" would be trivially true.
	if len(eligible) == 0 {
		t.Fatal("positive control FAILED: no rule is eligible for this artist at all")
	}

	rows, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	persisted := make(map[string]bool, len(rows))
	for i := range rows {
		persisted[rows[i].RuleID] = true
	}
	for _, id := range eligible {
		if !persisted[id] {
			t.Errorf("eligible rule %s has NO persisted rule_results row. It will count as "+
				"'missing' on every health recompute and offlineHealthScore will refuse to "+
				"score this artist forever.", id)
		}
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact, RuleNFOExists} {
		if persisted[id] {
			t.Errorf("skipped rule %s has a persisted rule_results row; a rule that never ran "+
				"must not be recorded as pass or fail", id)
		}
	}

	score, ok := p.offlineHealthScore(ctx, a, nil)
	if !ok {
		t.Fatal("offlineHealthScore REFUSED to score an API-only artist whose inapplicable " +
			"rules were skipped. Skipped rules must be out of the denominator; if they are " +
			"not, every path-less artist's health score freezes permanently (#2509).")
	}
	if score < 0 || score > 100 {
		t.Errorf("health score %v out of range", score)
	}
}

// TestArtistWithPath_DuplicateRulesStayEligible is the regression guard on the
// existing filesystem path: the capability gate must short-circuit on a local
// path and never consult stored hashes for such an artist. An artist with a
// directory and no image rows at all still has both duplicate rules EVALUATED,
// exactly as before this change, because its files can be read and hashed.
//
// Mutant this kills: dropping the `if a.Path != "" { return true }` short-circuit
// from the capability predicates. Every on-disk artist whose hashes are not yet
// persisted would then be skipped, and the rules would stop running on the
// filesystem libraries they were written for.
func TestArtistWithPath_DuplicateRulesStayEligible(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, _ := dupRuleEngine(t, db)

	a := &artist.Artist{Name: "On Disk", Path: t.TempDir()}
	if err := artist.NewService(db).Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact, RuleNFOExists} {
		if !slices.Contains(res.RulesConsidered, id) {
			t.Errorf("%s was not evaluated for an artist WITH a local path; skipped=%v",
				id, res.RulesSkipped)
		}
	}
	if len(res.RulesSkipped) != 0 {
		t.Errorf("an artist with a local path had rules skipped: %v", res.RulesSkipped)
	}
}

// TestEvaluationResult_RulesSkippedIsSerialized pins the JSON contract. The
// artist health endpoint embeds *rule.EvaluationResult, so this field name is the
// API surface a caller uses to tell "skipped" apart from "passed"; renaming or
// un-tagging it silently removes that distinction again.
func TestEvaluationResult_RulesSkippedIsSerialized(t *testing.T) {
	res := &EvaluationResult{
		ArtistID: "a1",
		RulesSkipped: []SkippedRule{
			{RuleID: RuleImageDuplicate, RuleName: "No duplicate images", Reason: SkipReasonNoComparablePerceptualHashes},
		},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"rules_skipped"`, `"rule_id":"image_duplicate"`, `"reason":`} {
		if !strings.Contains(got, want) {
			t.Errorf("serialized health response is missing %s; got %s", want, got)
		}
	}
}

// TestImageHashCapabilities_OneQueryPerArtistPerEvaluation pins the cost of the
// eligibility gate. It is consulted by BOTH duplicate rules and again by
// EligibleRuleIDs during the health recompute, so an uncached predicate would
// issue several identical queries per artist on every pass over the library.
func TestImageHashCapabilities_OneQueryPerArtistPerEvaluation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	engine, _ := dupRuleEngine(t, db)
	a := apiOnlyArtist(t, db, "API Cached")
	insertAPIImage(t, db, a.ID, "fanart", 0, 0xAAAAAAAAAAAAAAAA, "")
	insertAPIImage(t, db, a.ID, "fanart", 1, 0x5555555555555555, "")

	if _, err := engine.Evaluate(ctx, a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The cache survives the evaluation and is reused by the EligibleRuleIDs call
	// the offline recompute makes immediately afterwards.
	engine.imageCapMu.Lock()
	_, cached := engine.imageCapCache[a.ID]
	engine.imageCapMu.Unlock()
	if !cached {
		t.Fatal("the per-artist image-hash summary was not memoized, so each duplicate rule " +
			"and each EligibleRuleIDs call re-queries artist_images")
	}

	// A fresh evaluation must start from a clean cache, or a later pass would
	// serve stale eligibility after images changed.
	if _, err := engine.Evaluate(ctx, a); err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	engine.imageCapMu.Lock()
	entries := len(engine.imageCapCache)
	engine.imageCapMu.Unlock()
	if entries != 1 {
		t.Errorf("cache holds %d entries after a second evaluation of one artist; want 1 "+
			"(it must be cleared at the top of each EvaluateScoped)", entries)
	}
}

// dropTable removes a table outright: the honest way to make the very next query
// against it fail, with no mock in sight. The failure the production code sees is
// a real driver error from a real SQLite connection.
func dropTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `DROP TABLE `+table); err != nil {
		t.Fatalf("dropping %s to inject a query failure: %v", table, err)
	}
}

// TestCapabilityPredicates_NilDatabaseSkipsRatherThanPasses pins the ORDER of the
// two short-circuits in each predicate, which is the whole #2509 bug in miniature.
//
// The nil-database check comes FIRST, before the a.Path != "" fast path. An engine
// with no database handle cannot read artist_images, so it cannot compare anything
// for anybody -- including an artist WITH a local path. The rule must therefore be
// SKIPPED, and the skip must be reported with a reason so the retraction path
// withdraws any verdict a previously-wired engine left behind.
//
// Mutant this kills: moving the a.Path != "" check above the nil-db check. That
// would declare a path-having artist ELIGIBLE on a db-less engine, the checker's
// own `if e.db == nil { return nil }` guard would then fire, and nil from a Checker
// means "no violation" -- a fabricated PASS for a rule that never ran. That is
// precisely the class of false pass this issue exists to eliminate.
func TestCapabilityPredicates_NilDatabaseSkipsRatherThanPasses(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// A REAL service over a REAL database, but an engine with NO database handle.
	engine := NewEngine(svc, nil, nil, nil, testLogger())
	if engine.db != nil {
		t.Fatalf("precondition: engine must have a nil db handle for this test to mean anything")
	}

	predicates := map[string]func(context.Context, *artist.Artist) (bool, string, error){
		RuleImageDuplicate:      engine.capImageDuplicate,
		RuleImageDuplicateExact: engine.capImageDuplicateExact,
	}

	artists := map[string]*artist.Artist{
		// The path-having artist is the load-bearing case: it is the one the
		// a.Path != "" fast path would wrongly wave through.
		"artist with a local path": {ID: "cap-nil-db-path", Name: "Path Artist", Path: "/music/Path Artist"},
		"artist with no path":      {ID: "cap-nil-db-nopath", Name: "API Artist", Path: ""},
	}

	for _, ruleID := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		for label, a := range artists {
			t.Run(ruleID+"/"+label, func(t *testing.T) {
				capable, reason, err := predicates[ruleID](t.Context(), a)
				if err != nil {
					t.Fatalf("capability check returned an error; a nil db is a known state, "+
						"not a failure: %v", err)
				}
				if capable {
					t.Errorf("%s is ELIGIBLE for %q on an engine with no database. The checker "+
						"would then return nil (= no violation = PASS) from its own nil-db guard, "+
						"recording a pass for a rule that compared nothing.", ruleID, label)
				}
				if reason != SkipReasonNoDatabase {
					t.Errorf("skip reason = %q; want %q. The reason is what the retraction path "+
						"logs and what the UI shows; an empty reason reads as a silent drop.",
						reason, SkipReasonNoDatabase)
				}
			})
		}
	}
}

// TestCapabilityPredicates_DBErrorFailsClosed is the fail-closed contract for a
// TRANSIENT database failure, as distinct from the nil-db case above.
//
// A failed capability query means the engine does not KNOW whether the rule
// applies. There are exactly three things it could do, and two of them are wrong:
//
//   - report ELIGIBLE: the rule runs against data the engine could not read;
//   - report SKIPPED (capable=false with a reason): a skip is a POSITIVE claim
//     that the rule does not apply, and the retraction path acts on it -- it would
//     DELETE the artist's stored verdict and resolve its open violation on the
//     strength of a database hiccup. That destroys real findings.
//   - return the error: the caller aborts and the artist stays dirty for retry.
//
// Only the third is safe, so this asserts capable=false AND reason=="" AND err!=nil:
// an empty reason is what stops eligibleRules from ever appending a SkippedRule.
//
// Mutant this kills: swallowing the error as `return false, SkipReasonSomething, nil`,
// or as `return true, "", nil`.
func TestCapabilityPredicates_DBErrorFailsClosed(t *testing.T) {
	for _, ruleID := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		t.Run(ruleID, func(t *testing.T) {
			db := setupTestDB(t)
			engine, _ := dupRuleEngine(t, db)
			predicate := engine.capImageDuplicate
			if ruleID == RuleImageDuplicateExact {
				predicate = engine.capImageDuplicateExact
			}

			a := apiOnlyArtist(t, db, "API Cap Error")
			insertBlindFanartPair(t, db, a.ID)

			// POSITIVE CONTROL. Before the injection the predicate reaches the
			// query and answers "skipped, nothing comparable". Without this the
			// assertions below could pass on a predicate that never queried at all.
			capable, reason, err := predicate(t.Context(), a)
			if err != nil {
				t.Fatalf("positive control: capability check failed on a healthy db: %v", err)
			}
			if capable || reason == "" {
				t.Fatalf("positive control FAILED: on a healthy db this artist must be reported "+
					"SKIPPED with a reason, got capable=%v reason=%q; the injection below would "+
					"then prove nothing", capable, reason)
			}
			// The cache would answer the next call from memory and never touch the
			// dropped table. Clear it, exactly as EvaluateScoped does on entry.
			engine.imageCapMu.Lock()
			engine.imageCapCache = nil
			engine.imageCapMu.Unlock()

			dropTable(t, db, "artist_images")

			capable, reason, err = predicate(t.Context(), a)
			if err == nil {
				t.Fatalf("capability check SWALLOWED a database failure (capable=%v reason=%q). "+
					"It must propagate: the engine does not know whether the rule applies.",
					capable, reason)
			}
			if !strings.Contains(err.Error(), "querying image hashes for rule eligibility") {
				t.Errorf("error = %v; want it to name the failing capability query so an "+
					"operator can tell it from a checker failure", err)
			}
			if capable {
				t.Errorf("capability check returned ELIGIBLE alongside an error; the rule would " +
					"run against data the engine could not read")
			}
			if reason != "" {
				t.Errorf("capability check returned skip reason %q alongside an error. A skip is "+
					"a positive claim that the rule does not apply, and RETRACTION ACTS ON IT: "+
					"a transient query failure would delete this artist's stored verdict and "+
					"resolve its open violation. The reason must stay empty on error.", reason)
			}
		})
	}
}

// TestEvaluate_CapabilityErrorAbortsEvaluationWithoutVerdict carries the fail-closed
// contract above up to the engine boundary, which is where it actually protects data.
//
// eligibleRules must ABORT the whole evaluation on a capability error rather than
// dropping the rule from the run. Evaluate returns no result at all, so no caller
// can persist a pass, and no caller can retract anything either.
//
// Mutant this kills: `if capErr != nil { continue }` in eligibleRules, which silently
// drops the rule from RulesTotal and leaves the artist scored against a smaller
// denominator with nobody the wiser.
func TestEvaluate_CapabilityErrorAbortsEvaluationWithoutVerdict(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, _ := dupRuleEngine(t, db)

	a := apiOnlyArtist(t, db, "API Evaluate Cap Error")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// POSITIVE CONTROL: on a healthy db this artist evaluates and reports the
	// duplicate rules as skipped, so the capability gate is genuinely on the path.
	pre, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("positive control: Evaluate failed on a healthy db: %v", err)
	}
	if _, skipped := skipReasonFor(pre, RuleImageDuplicate); !skipped {
		t.Fatalf("positive control FAILED: %s was not skipped, so the capability gate is not "+
			"on this artist's path and the injection below proves nothing", RuleImageDuplicate)
	}

	dropTable(t, db, "artist_images")

	res, err := engine.Evaluate(ctx, a)
	if err == nil {
		t.Fatalf("Evaluate SUCCEEDED after its capability query failed; it must abort. "+
			"Result: %+v", res)
	}
	if res != nil {
		t.Errorf("Evaluate returned a non-nil result alongside an error: %+v. A partial result "+
			"invites a caller to persist verdicts derived from a failed gate.", res)
	}
	if !strings.Contains(err.Error(), "checking capability for rule") {
		t.Errorf("error = %v; want it to name the rule whose capability check failed", err)
	}

	// The stale rows are UNTOUCHED. An aborted evaluation must neither confirm a
	// verdict nor retract one: the artist stays dirty and is retried.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		passed, exists := ruleResultRow(t, db, a.ID, id)
		if !exists {
			t.Errorf("the failed evaluation RETRACTED %s's stored row. A database error is not "+
				"evidence that the rule does not apply, and retracting on one destroys verdicts "+
				"on every hiccup.", id)
			continue
		}
		if !passed {
			t.Errorf("the failed evaluation rewrote %s's stored row (passed=false)", id)
		}
	}
}

// TestEligibleRules_RuleWithoutCheckerIsNeitherEligibleNorSkipped guards the
// boundary between "this rule cannot apply to THIS ARTIST" and "this engine build
// has no code for this rule".
//
// RulesSkipped is a per-artist statement, and it is consumed by RETRACTION: every
// entry in it has the artist's stored verdict for that rule withdrawn. A rule with
// no registered checker is an engine/config state that says nothing about the
// artist, so it must not appear there -- otherwise upgrading to a build that
// temporarily lacks a checker would silently delete that rule's verdicts library-wide.
// It is dropped from the run entirely: not eligible, not skipped, not counted.
//
// Mutant this kills: turning the `continue` in the no-checker branch into a
// `skipped = append(...)`.
func TestEligibleRules_RuleWithoutCheckerIsNeitherEligibleNorSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()

	const orphanRule = "test_rule_with_no_checker"
	_, err := db.ExecContext(ctx,
		`INSERT INTO rules (id, name, description, category, enabled, config, automation_mode)
		 VALUES (?, 'Orphan Rule', 'enabled in the DB, no checker in this build', 'metadata', 1, '{}', 'manual')`,
		orphanRule)
	if err != nil {
		t.Fatalf("seeding the checker-less rule: %v", err)
	}

	engine, svc := dupRuleEngine(t, db)

	// PRECONDITIONS. The rule is ENABLED (a disabled rule is dropped by an earlier
	// branch, which would make this test exercise the wrong `continue`), and the
	// engine really has no checker for it.
	stored, err := svc.GetByID(ctx, orphanRule)
	if err != nil {
		t.Fatalf("loading the checker-less rule: %v", err)
	}
	if !stored.Enabled {
		t.Fatalf("precondition: the orphan rule must be ENABLED to reach the no-checker branch")
	}
	if _, ok := engine.checkers[orphanRule]; ok {
		t.Fatalf("precondition: a checker is registered for %s, so the no-checker branch is "+
			"never taken and this test is vacuous", orphanRule)
	}

	a := apiOnlyArtist(t, db, "API Orphan Rule")
	insertBlindFanartPair(t, db, a.ID)

	eligible, skipped, err := engine.eligibleRules(ctx, a)
	if err != nil {
		t.Fatalf("eligibleRules: %v", err)
	}

	// POSITIVE CONTROL: the loop ran and did classify rules. Without this, "the
	// orphan is in neither list" passes on an engine that returned two empty slices.
	if len(eligible) == 0 {
		t.Fatalf("positive control FAILED: no rule is eligible for this artist, so 'the orphan " +
			"rule is not eligible' proves nothing")
	}
	if len(skipped) == 0 {
		t.Fatalf("positive control FAILED: no rule is skipped for this artist, so 'the orphan " +
			"rule is not skipped' proves nothing")
	}

	for i := range eligible {
		if eligible[i].ID == orphanRule {
			t.Errorf("%s is ELIGIBLE despite having no registered checker; the engine would "+
				"count it in RulesTotal and never run it", orphanRule)
		}
	}
	for _, s := range skipped {
		if s.RuleID == orphanRule {
			t.Errorf("%s is reported as SKIPPED (reason %q). RulesSkipped drives RETRACTION: "+
				"a build that happens to lack this rule's checker would then DELETE every "+
				"artist's stored verdict for it. A missing checker is an engine state, not a "+
				"statement about this artist.", orphanRule, s.Reason)
		}
	}
}
