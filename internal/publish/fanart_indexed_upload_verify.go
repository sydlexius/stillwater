package publish

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// indexedUploadLagTolerance bounds every verify read (#3126), matching the
// live #3150 test's pollBackdropDetail budget: Emby's BackdropImageTags settle
// within a few seconds of a write. A var so unit tests can shrink it.
var indexedUploadLagTolerance = 3 * time.Second

// indexedUploadPollInterval is the re-read cadence (pollBackdropDetail's). A
// var for the same test-speed reason.
var indexedUploadPollInterval = 250 * time.Millisecond

// pollStableCount re-reads the peer's backdrop count until it has held STILL
// across two consecutive reads at or above want, waiting out Emby's measured
// lag on BackdropImageTags (a single early read can report the pre-write
// count). ok=false means the budget (indexedUploadLagTolerance, which also
// bounds each read) ran out first. The count is only a cheap first filter; see
// verifyIndexedUploadLanded.
func pollStableCount(ctx context.Context, reader connection.ArtistStateGetter, platformArtistID string, want int) (count int, ok bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, indexedUploadLagTolerance)
	defer cancel()
	stable, last := 0, -1
	for {
		state, getErr := reader.GetArtistDetail(pctx, platformArtistID)
		if getErr != nil {
			if pctx.Err() != nil && ctx.Err() == nil {
				return last, false, nil // budget spent mid-read
			}
			return last, false, fmt.Errorf("re-reading backdrop count to verify indexed upload: %w", getErr)
		}
		if last != -1 && state.BackdropCount == last {
			stable++
		} else {
			stable = 0
		}
		last = state.BackdropCount
		if stable >= 1 && last >= want {
			return last, true, nil
		}
		select {
		case <-pctx.Done():
			return last, false, ctx.Err() // nil unless the CALLER's ctx ended
		case <-time.After(indexedUploadPollInterval):
		}
	}
}

// peerContentHashes returns the exact content hash of each of the peer's first
// count backdrops. Each fetch gets its own indexedUploadLagTolerance budget (a
// peer with many backdrops must not exhaust one shared budget and read as
// "absent"), and the whole scan is bounded at ten budgets. A fetch error is
// returned, never read as absence: a blind spot could hide the very slot being
// looked for.
func peerContentHashes(ctx context.Context, reader connection.BackdropReader, platformArtistID string, count int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*indexedUploadLagTolerance)
	defer cancel()
	hashes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		fctx, fcancel := context.WithTimeout(ctx, indexedUploadLagTolerance)
		got, _, err := reader.GetArtistBackdrop(fctx, platformArtistID, i)
		fcancel()
		if err != nil {
			return nil, fmt.Errorf("fetching backdrop %d: %w", i, err)
		}
		hashes = append(hashes, img.ContentHash(got))
	}
	return hashes, nil
}

// verifyIndexedUploadLanded decides, after an indexed upload returned an HTTP
// error, whether the peer accepted the write anyway (#3126: Emby 500s on an
// out-of-range upload yet appends). The verdict is by CONTENT: the count must
// first settle at wantAtLeast or more (cheap filter, lag-tolerant), then the
// uploaded bytes must be present among the peer's backdrops. A count alone
// cannot say whether THIS upload landed (a stale baseline or a concurrent
// append moves it), so it never decides by itself.
//
// landed=false is a definite "not there" and the caller reports the original
// error. Any read failure returns err, which the caller treats the same way:
// a transient read failure never suppresses a real upload failure.
func verifyIndexedUploadLanded(ctx context.Context, reader connection.BackdropReader, platformArtistID string, wantAtLeast int, data []byte) (landed bool, count int, err error) {
	count, ok, err := pollStableCount(ctx, reader, platformArtistID, wantAtLeast)
	if err != nil || !ok {
		return false, count, err
	}
	hashes, err := peerContentHashes(ctx, reader, platformArtistID, count)
	if err != nil {
		return false, count, err
	}
	return slices.Contains(hashes, img.ContentHash(data)), count, nil
}
