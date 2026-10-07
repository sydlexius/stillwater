package publish

import (
	"context"
	"errors"
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
	count, ok, _, err = pollCount(ctx, reader, platformArtistID, want)
	return count, ok, err
}

// pollCount is pollStableCount plus one more answer: settled. "Settled" means
// the LAST two reads were equal, i.e. the count had stopped moving when the
// budget ended. ok=false with settled=true is a count holding still BELOW want
// (the peer really did not take the write). ok=false with settled=false is a
// count still moving when time ran out: the verdict is unknown, not "no"
// (#3200).
func pollCount(ctx context.Context, reader connection.ArtistStateGetter, platformArtistID string, want int) (count int, ok, settled bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, indexedUploadLagTolerance)
	defer cancel()
	stable, last := 0, -1
	for {
		state, getErr := reader.GetArtistDetail(pctx, platformArtistID)
		if getErr != nil {
			if pctx.Err() != nil && ctx.Err() == nil {
				return last, false, stable >= 1, nil // budget spent mid-read
			}
			return last, false, false, fmt.Errorf("re-reading backdrop count to verify indexed upload: %w", getErr)
		}
		if last != -1 && state.BackdropCount == last {
			stable++
		} else {
			stable = 0
		}
		last = state.BackdropCount
		if stable >= 1 && last >= want {
			return last, true, true, nil
		}
		select {
		case <-pctx.Done():
			return last, false, stable >= 1, ctx.Err() // err is nil unless the CALLER's ctx ended
		case <-time.After(indexedUploadPollInterval):
		}
	}
}

// peerContentHashes returns the exact content hash of the peer's backdrops at
// indices [from, to). Each fetch gets its own indexedUploadLagTolerance budget
// (a peer with many backdrops must not exhaust one shared budget and read as
// "absent"), and the whole scan is bounded at ten budgets. A fetch error is
// returned, never read as absence: a blind spot could hide the very slot being
// looked for.
func peerContentHashes(ctx context.Context, reader connection.BackdropReader, platformArtistID string, from, to int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*indexedUploadLagTolerance)
	defer cancel()
	hashes := make([]string, 0, to-from)
	for i := from; i < to; i++ {
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

// peerCache is ONE per-push picture of the peer's backdrop content, shared by
// the duplicate guard and the landed-upload check so the peer is read once per
// push, not once per upload (#3126). It is loaded lazily from a SETTLED count
// (a stale first read must not shorten it), and afterwards extended only by
// what the push itself appends.
type peerCache struct {
	reader connection.BackdropReader
	id     string
	hashes []string
	loaded bool
}

// load reads the settled count, then hashes every backdrop. An error leaves
// the cache unloaded; the caller disables everything that depends on it.
func (c *peerCache) load(ctx context.Context) error {
	count, ok, err := pollStableCount(ctx, c.reader, c.id, 0)
	if err == nil && !ok {
		err = errors.New("backdrop count never settled")
	}
	if err != nil {
		return err
	}
	if c.hashes, err = peerContentHashes(ctx, c.reader, c.id, 0, count); err != nil {
		return err
	}
	c.loaded = true
	return nil
}

func (c *peerCache) holds(data []byte) bool {
	return slices.Contains(c.hashes, img.ContentHash(data))
}

// errVerifyUnsettled is a distinct error value so the log line says WHY the
// verify failed: the budget ran out before two equal reads (the count still
// moving, or no read completing), so whether the write landed is UNKNOWN. That
// differs from a count that settled below the wanted value (a plain "not
// landed"). confirmLanded treats it like any other read failure; nothing
// branches on it.
var errVerifyUnsettled = errors.New("backdrop count did not settle (or could not be read) within the budget; the upload could not be verified")

// landedSince decides, after an indexed upload returned an HTTP error, whether
// the peer accepted the write anyway (#3126: Emby 500s on an out-of-range
// upload yet appends). It is by CONTENT and by NEWNESS: the settled count must
// grow past the cached baseline, and the bytes must appear at an index at or
// past it. Bytes already in the baseline (a stale seed, an earlier identical
// slot) therefore never confirm a genuine failure. The new entries are added to
// the cache. A read failure returns err, which the caller treats as "report the
// original error". (A concurrent append of byte-identical content during the
// window cannot be told apart from our own write; that is accepted.) A count
// that never SETTLED in the budget returns errVerifyUnsettled: unverifiable, so
// the cache is stale and the caller must not upload more on a guess (#3200). A
// count that settled below the target returns no error and no hashes: a
// genuine "not landed".
func (c *peerCache) landedSince(ctx context.Context) (hashes []string, count int, err error) {
	count, ok, settled, err := pollCount(ctx, c.reader, c.id, len(c.hashes)+1)
	if err != nil {
		return nil, count, err
	}
	if !ok {
		if !settled {
			return nil, count, errVerifyUnsettled
		}
		return nil, count, nil
	}
	hashes, err = peerContentHashes(ctx, c.reader, c.id, len(c.hashes), count)
	if err != nil {
		return nil, count, err
	}
	c.hashes = append(c.hashes, hashes...)
	return hashes, count, nil
}
