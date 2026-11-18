package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	img "github.com/sydlexius/stillwater/internal/image"
)

// Defaults for the platform hash backfill (#2513).
const (
	defaultPlatformBackfillPerMinute = 120
	defaultPlatformBackfillMaxPass   = 200
	platformFailureCooldownPasses    = 4
)

// PlatformSlotFetcher is the pass's only network seam (*imagebridge.Bridge).
type PlatformSlotFetcher interface {
	FetchArtistImageSlot(ctx context.Context, artistID, imageType string, slotIndex int) ([]byte, string, error)
}

// PlatformHashBackfillOpts bounds one pass.
type PlatformHashBackfillOpts struct {
	RequestsPerMinute int // platform fetch rate ceiling (default 120)
	MaxPerPass        int // fetches attempted per pass (default 200); the rest wait for the next pass
	// Names reports the active profile's filenames for a type (an error leaves the row unverified); nil = defaults only.
	Names func(ctx context.Context, imageType string) ([]string, error)
}

// PlatformHashBackfillResult reports one pass. Selected counts platform fetches only; Changed, rows another writer hashed first.
type PlatformHashBackfillResult struct {
	Selected, Filled, Failed, Changed int
	Truncated                         bool
	RequestsPerMinute, MaxPerPass     int
}

type platformHashRow struct {
	artistID, imageType, phash, contentHash string
	slotIndex                               int
}

// before reports whether r sorts before o in the candidate query's order.
func (r platformHashRow) before(o platformHashRow) bool {
	if r.artistID != o.artistID {
		return r.artistID < o.artistID
	}
	if r.imageType != o.imageType {
		return r.imageType < o.imageType
	}
	return r.slotIndex < o.slotIndex
}

func (r platformHashRow) key() string {
	return fmt.Sprintf("%s/%s/%d", r.artistID, r.imageType, r.slotIndex)
}

// platformHashCooldown keeps rows that never succeed from starving healthy ones
// of the per-pass cap; a failed row sits out platformFailureCooldownPasses passes.
//
// It also holds the fairness cursor: a pass that stops on the cap resumes at
// that row next time (resume), so a long run of failing rows ahead of a healthy
// one is walked past rather than retried from the top each cooldown cycle.
type platformHashCooldown struct {
	mu     sync.Mutex
	pass   int
	until  map[string]int
	resume *platformHashRow // first row not reached by a capped pass; nil = start at the top
}

// startIndex is the position in the (artist, type, slot)-ordered candidates to
// start at: the first row at or after the resume point (wrapping to 0 if none).
func (c *platformHashCooldown) startIndex(rows []platformHashRow) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resume == nil {
		return 0
	}
	for i, r := range rows {
		if !r.before(*c.resume) {
			return i
		}
	}
	return 0
}

func (c *platformHashCooldown) setResume(r *platformHashRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resume = r
}

func (c *platformHashCooldown) begin() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pass++
	return c.pass
}

func (c *platformHashCooldown) cooling(key string, pass int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until[key] > pass
}

func (c *platformHashCooldown) record(key string, pass int, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !failed {
		delete(c.until, key)
		return
	}
	if c.until == nil {
		c.until = map[string]int{}
	}
	c.until[key] = pass + platformFailureCooldownPasses + 1
}

// BackfillPlatformImageHashes fills phash and content_hash for the single-image
// rows (thumb, logo, banner) of API-only artists (no library path, mapped to an
// enabled Emby or Jellyfin connection) that lack them.
//
// Fanart is excluded (#3602): its slot_index is a local ordinal that need not
// match the platform's backdrop index. Per row:
//   - A local file (profile names plus the built-in defaults) is hashed; nothing
//     is fetched and no rate-limit token is spent.
//   - With none, the bytes come from the artist's pinned platform connection. A
//     MISSING cache directory counts as "no local file".
//   - A directory or profile that cannot be read fails CLOSED: unverifiable,
//     nothing fetched ("cannot tell" is not "absent").
//   - The write is a compare-and-set; a row another writer hashed meanwhile is
//     counted in Changed and left alone.
//
// A failed fetch, empty body or undecodable image leaves the row unhashed and on
// cooldown. Path-backed artists are not selected (#3600).
func (s *Service) BackfillPlatformImageHashes(
	ctx context.Context, fetcher PlatformSlotFetcher, opts PlatformHashBackfillOpts,
) (*PlatformHashBackfillResult, error) {
	if fetcher == nil {
		return nil, errors.New("backfilling platform image hashes: fetcher is required")
	}
	if opts.RequestsPerMinute <= 0 {
		opts.RequestsPerMinute = defaultPlatformBackfillPerMinute
	}
	if opts.MaxPerPass <= 0 {
		opts.MaxPerPass = defaultPlatformBackfillMaxPass
	}
	res := &PlatformHashBackfillResult{RequestsPerMinute: opts.RequestsPerMinute, MaxPerPass: opts.MaxPerPass}
	candidates, err := s.selectPlatformHashRows(ctx)
	if err != nil {
		return nil, err
	}
	pass := s.platformHash.begin() // burst-1 limiter below: requests are evenly spaced
	limiter := rate.NewLimiter(rate.Limit(float64(opts.RequestsPerMinute)/60.0), 1)
	start := s.platformHash.startIndex(candidates)
	s.platformHash.setResume(nil) // a pass that finishes the list leaves the cursor at the top
	for n := range candidates {
		row := candidates[(start+n)%len(candidates)]
		if s.platformHash.cooling(row.key(), pass) {
			continue
		}
		phash, content, found, reason := s.localRowHashes(ctx, opts.Names, row)
		if reason == "" && !found {
			if res.Selected == opts.MaxPerPass {
				res.Truncated = true
				s.platformHash.setResume(&row)
				break
			}
			res.Selected++
			if err := limiter.Wait(ctx); err != nil {
				return res, fmt.Errorf("platform hash backfill canceled after %d rows filled: %w", res.Filled, err)
			}
			phash, content, reason = s.fetchedRowHashes(ctx, fetcher, row)
		}
		var stored bool
		if reason == "" {
			stored, reason = s.storeHashes(ctx, row, phash, content)
			if stored && reason == "" && phash == "" {
				// The content hash is kept, but an unknown phash leaves the row unfilled.
				reason = "perceptual hash unusable (zero is the unknown sentinel)"
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, fmt.Errorf("platform hash backfill canceled after %d rows filled: %w", res.Filled, ctxErr)
		}
		s.platformHash.record(row.key(), pass, reason != "")
		switch {
		case reason != "":
			res.Failed++
			s.logger.Warn("platform hash backfill: row left unhashed",
				slog.String("artist_id", row.artistID), slog.String("image_type", row.imageType),
				slog.String("reason", reason))
		case stored:
			res.Filled++
		default:
			res.Changed++
		}
	}
	s.logger.Info("platform hash backfill pass complete", slog.Any("result", res))
	return res, nil
}

// selectPlatformHashRows drains every candidate before any write (single-writer
// SQLite). The EXISTS admits only Emby and Jellyfin, the set the bridge pins from.
func (s *Service) selectPlatformHashRows(ctx context.Context) ([]platformHashRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ai.artist_id, ai.image_type, ai.slot_index, ai.phash, ai.content_hash
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		WHERE ai.exists_flag = 1 AND (ai.phash = '' OR ai.content_hash = '')
		  AND ai.image_type IN ('thumb', 'logo', 'banner')
		  AND a.path = ''
		  AND EXISTS (SELECT 1 FROM artist_platform_ids p JOIN connections c ON c.id = p.connection_id
		              WHERE p.artist_id = a.id AND c.enabled = 1 AND c.type IN ('emby', 'jellyfin'))
		ORDER BY ai.artist_id, ai.image_type, ai.slot_index`)
	if err != nil {
		return nil, fmt.Errorf("querying platform image rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, no actionable close error
	var out []platformHashRow
	for rows.Next() {
		var r platformHashRow
		if err := rows.Scan(&r.artistID, &r.imageType, &r.slotIndex, &r.phash, &r.contentHash); err != nil {
			return nil, fmt.Errorf("scanning unhashed platform image row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating unhashed platform image rows: %w", err)
	}
	return out, nil
}

// storeHashes is the compare-and-set write over both hash columns: (true, "")
// written, (false, "") another writer got there first, (false, reason) failed.
func (s *Service) storeHashes(ctx context.Context, row platformHashRow, phash, content string) (bool, string) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE artist_images SET phash = ?, content_hash = ?
		WHERE artist_id = ? AND image_type = ? AND slot_index = ? AND phash = ? AND content_hash = ?`,
		phash, content, row.artistID, row.imageType, row.slotIndex, row.phash, row.contentHash)
	if err != nil {
		return false, "storing hashes failed: " + err.Error()
	}
	n, _ := res.RowsAffected()
	return n > 0, ""
}

// localRowHashes hashes the row's local file if one exists, looked up under the
// profile's names plus the built-in defaults. found=false with no reason means
// no such file (a missing directory included); any other inability to tell is a reason.
func (s *Service) localRowHashes(
	ctx context.Context, namesFn func(context.Context, string) ([]string, error), row platformHashRow,
) (phash, content string, found bool, reason string) {
	dir := s.artistImageDir("", row.artistID)
	if dir == "" {
		return "", "", false, "local image directory unresolved: cannot verify absence"
	}
	var names []string
	if namesFn != nil {
		configured, err := namesFn(ctx, row.imageType)
		if err != nil {
			return "", "", false, "reading profile image names failed: " + err.Error()
		}
		names = append(names, configured...)
	}
	names = append(names, img.DefaultFileNames[row.imageType]...)
	p, ok, err := img.FindExistingImageStrict(ctx, dir, names)
	if err != nil {
		return "", "", false, "inspecting local image failed: " + err.Error()
	}
	if !ok {
		return "", "", false, ""
	}
	fh, err := img.HashFile(ctx, p, true)
	if err != nil {
		return "", "", false, "hashing local image failed: " + err.Error()
	}
	return keepPhash(row, perceptualHex(fh.Perceptual)), fh.Content, true, ""
}

func (s *Service) fetchedRowHashes(
	ctx context.Context, fetcher PlatformSlotFetcher, row platformHashRow,
) (phash, content, reason string) {
	data, _, err := fetcher.FetchArtistImageSlot(ctx, row.artistID, row.imageType, row.slotIndex)
	if err != nil {
		return "", "", "fetch failed: " + err.Error()
	}
	if len(data) == 0 {
		return "", "", "platform returned an empty body"
	}
	ph, err := img.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		return "", "", "decode failed: " + err.Error()
	}
	return keepPhash(row, perceptualHex(ph)), img.ContentHash(data), ""
}

// perceptualHex renders a perceptual hash; 0 is the "unknown" sentinel the
// rules read as unhashed, so it is stored as the empty string, never "000...0".
func perceptualHex(h uint64) string {
	if h == 0 {
		return ""
	}
	return img.HashHex(h)
}

// keepPhash keeps a phash the save path recorded (it may be EXIF-aware).
func keepPhash(row platformHashRow, computed string) string {
	if row.phash != "" {
		return row.phash
	}
	return computed
}

// StartPlatformHashBackfill runs the pass after startupDelay, then every
// interval until ctx is canceled (new platform images arrive unhashed).
func (s *Service) StartPlatformHashBackfill(
	ctx context.Context, fetcher PlatformSlotFetcher,
	opts PlatformHashBackfillOpts, interval, startupDelay time.Duration,
) {
	if interval <= 0 {
		interval = 6 * time.Hour // time.NewTicker panics on <= 0
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.BackfillPlatformImageHashes(ctx, fetcher, opts); err != nil {
			s.logger.Error("platform hash backfill failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
