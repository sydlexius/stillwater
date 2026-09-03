package image

import "testing"

// TestMaxStalledReads_IsPinned states, as an executable sentence, this
// package's process-wide abandoned-read cap.
//
// It exists because internal/publish's advisory ceiling
// (maxAdvisoryStalledReads) cannot see this unexported constant and restates
// its value as a local literal instead -- see
// internal/publish/extrafanart_ordering_test.go's
// TestMaxAdvisoryStalledReads_IsBelowTheProcessCap. That restatement's own
// comment used to claim it "notices if the two drift apart", which it
// cannot: a change here leaves that copy comparing itself against itself.
// This test is where the drift is actually caught -- change maxStalledReads
// and this test reddens with the real value in hand, which is the signal to
// go update the publish-side restatement in lockstep.
func TestMaxStalledReads_IsPinned(t *testing.T) {
	if maxStalledReads != 16 {
		t.Fatalf("maxStalledReads = %d, want 16 -- if this change is deliberate, also update the "+
			"restated copy in internal/publish/extrafanart_ordering_test.go "+
			"(TestMaxAdvisoryStalledReads_IsBelowTheProcessCap), which cannot see this constant and "+
			"will not notice on its own", maxStalledReads)
	}
}
