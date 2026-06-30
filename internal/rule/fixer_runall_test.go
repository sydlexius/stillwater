package rule

import (
	"testing"
)

// TestMergeIntoContrib exercises every branch of the mergeIntoContrib helper,
// which is the per-violation merge path used by processArtistForRunAll. The
// imageFix and persistFailed branches cannot be reached by the integration
// tests in fixer_parallel_test.go (they require a successful image fix or a
// DB error in the upsert path respectively), so this unit test covers them
// directly.
func TestMergeIntoContrib(t *testing.T) {
	t.Run("fr_nil_persistFailed", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		acc.mergeIntoContrib(violationOutcome{persistFailed: true}, &contrib)
		if acc.persistOK {
			t.Error("persistOK should be false after persistFailed outcome")
		}
		if contrib.fixesAttempted != 0 || contrib.fixesSucceeded != 0 {
			t.Error("no fixer counters should increment for a nil-fr outcome")
		}
	})

	t.Run("fixed_imageFix", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		fr := &FixResult{RuleID: "img_rule", Fixed: true, ImageType: "thumb"}
		rv := &RuleViolation{RuleID: "img_rule"}
		acc.mergeIntoContrib(violationOutcome{
			fr: fr, fixed: true, imageFix: true, imageType: "thumb",
			resolvedRow: rv,
		}, &contrib)
		if !acc.artistDirty {
			t.Error("artistDirty should be set on a successful fix")
		}
		if len(acc.fixedImageTypes) != 1 || acc.fixedImageTypes[0] != "thumb" {
			t.Errorf("fixedImageTypes = %v, want [thumb]", acc.fixedImageTypes)
		}
		if acc.metadataFixed {
			t.Error("metadataFixed should not be set for an image fix")
		}
		if contrib.fixesSucceeded != 1 {
			t.Errorf("fixesSucceeded = %d, want 1", contrib.fixesSucceeded)
		}
		if len(acc.resolvedRows) != 1 || acc.resolvedRows[0] != rv {
			t.Error("resolvedRow should be stashed in acc.resolvedRows")
		}
	})

	t.Run("fixed_metadata", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		fr := &FixResult{RuleID: "bio_rule", Fixed: true}
		acc.mergeIntoContrib(violationOutcome{fr: fr, fixed: true}, &contrib)
		if !acc.metadataFixed {
			t.Error("metadataFixed should be set for a non-image fix")
		}
		if len(acc.fixedImageTypes) != 0 {
			t.Error("fixedImageTypes should be empty for a metadata fix")
		}
	})
}
