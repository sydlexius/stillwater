package aiblock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/version"
)

// SourceURL is the upstream list: laylavish/uBlockOrigin-HUGE-AI-Blocklist,
// list_uBlacklist.txt, licensed CC0-1.0.
const SourceURL = "https://raw.githubusercontent.com/laylavish/uBlockOrigin-HUGE-AI-Blocklist/main/list_uBlacklist.txt"

const (
	// DefaultRefreshInterval is how often a running Store re-fetches the list.
	DefaultRefreshInterval = 24 * time.Hour
	// CacheFileName is the file the accepted list is persisted to in CacheDir.
	CacheFileName = "ai_blocklist.txt"
	// Caps on an accepted list. The real list is about 100 KiB with about 90
	// regexes; a list over any cap is rejected whole, never truncated, so a
	// hostile upstream cannot make Parse retain gigabytes of compiled regexes.
	maxListBytes  = 1 << 20
	maxRegexes    = 1000
	maxRegexBytes = 1024
	fetchTimeout  = 30 * time.Second
	// DefaultRetryInitial is the first delay after a failed refresh; it doubles
	// up to DefaultRetryMax, so a host that boots before its network is up
	// recovers within minutes instead of waiting a full DefaultRefreshInterval.
	DefaultRetryInitial = time.Minute
	// DefaultRetryMax caps the retry backoff.
	DefaultRetryMax = time.Hour
	// shrinkFloorPercent is the smallest share of the active list's rules a
	// replacement must keep. The upstream list only grows or is pruned a little
	// at a time, so a list under half the size is a truncation, a rewrite or a
	// hostile swap; half leaves room for a real cleanup and still catches a
	// gutted list. It never applies to the first load (nothing is active).
	shrinkFloorPercent = 50
)

// errCacheWrite marks a refresh whose list was valid and is now active in
// memory, but could not be persisted.
var errCacheWrite = errors.New("list is active but was not cached")

// errOversize is a list over maxListBytes. A refetch returns the same list, so
// unlike a transport failure it is not retried on the backoff.
var errOversize = fmt.Errorf("list exceeds %d bytes", maxListBytes)

// fetchErr marks a transport or HTTP failure, the only kind a retry can fix.
type fetchErr struct{ error }

func (e fetchErr) Unwrap() error { return e.error }

// emptyMatcher is the matcher before any list has loaded: it blocks nothing.
var emptyMatcher = &Matcher{}

// canaryURLs are ordinary image URLs Stillwater relies on. A list that blocks
// any of them is broken or hostile (e.g. "/./", or a public-suffix rule like
// "*://*.co.uk/*") and is rejected rather than hiding every search result.
var canaryURLs = [...]string{
	"https://upload.wikimedia.org/wikipedia/commons/a/a9/Example.jpg",
	"https://coverartarchive.org/release/76df3287-6cda-33eb-8e9a-044b5e15ffdd/829521842.jpg",
	"https://i.discogs.com/R-1234-1600000000.jpeg",
	"https://www.example.co.uk/images/artist.jpg",
	// Hosts Stillwater's image providers serve from.
	"https://assets.fanart.tv/fanart/music/5b11f4ce-a62d-471e-81fc-a69a8278c7da/artistbackground/nirvana-4f5e1c3b6a1d3.jpg",
	"https://r2.theaudiodb.com/images/media/artist/thumb/xxxx1234567890.jpg",
	"https://cdn-images.dzcdn.net/images/artist/0123456789abcdef0123456789abcdef/1000x1000-000000-80-0-0.jpg",
	"https://lastfm.freetls.fastly.net/i/u/770x0/0123456789abcdef0123456789abcdef.jpg",
	"https://i.scdn.co/image/ab6761610000e5eb0123456789abcdef01234567",
	"https://commons.wikimedia.org/wiki/Special:FilePath/Example.jpg",
	"https://e-cdns-images.dzcdn.net/images/artist/0123456789abcdef0123456789abcdef/500x500-000000-80-0-0.jpg",
	"https://ia800000.us.archive.org/0/items/mbid-76df3287-6cda-33eb-8e9a-044b5e15ffdd/mbid-76df3287-6cda-33eb-8e9a-044b5e15ffdd-829521842.jpg",
	"https://images.genius.com/0123456789abcdef0123456789abcdef.1000x1000x1.jpg",
}

// defaultStore backs Default(); see SetDefault.
var defaultStore atomic.Pointer[Store]

// DefaultStore returns the store installed with SetDefault, or nil.
func DefaultStore() *Store { return defaultStore.Load() }

// SetDefault installs s as the store Default() reads from.
func SetDefault(s *Store) { defaultStore.Store(s) }

// Snapshot returns the installed store's active matcher together with the
// Status describing it, read under one lock, so a caller filtering with the
// matcher reports the state of that same list (a load finishing mid-request
// cannot pair an empty matcher with Loaded=true). With no store installed it
// is the empty matcher and the zero Status (not loaded).
func Snapshot() (*Matcher, Status) {
	if s := defaultStore.Load(); s != nil {
		return s.snapshot()
	}
	return emptyMatcher, Status{}
}

// Options configures a Store. The zero value is usable: no disk cache, the
// upstream SourceURL, a 24h refresh, and the SSRF-guarded shared client.
type Options struct {
	// CacheDir holds the last accepted list across restarts. Empty disables
	// the cache.
	CacheDir string
	// Interval between refreshes; zero means DefaultRefreshInterval.
	Interval time.Duration
	// Client performs the fetch; nil means httpsafe.SafeClient.
	Client *http.Client
	// RetryInitial and RetryMax bound the backoff after a failed refresh; zero
	// means DefaultRetryInitial and DefaultRetryMax.
	RetryInitial, RetryMax time.Duration
	// URL overrides SourceURL (tests).
	URL    string
	Logger *slog.Logger
	// Disabled marks a store whose download the operator turned off. It is
	// only reported through Status, so callers can say "off" rather than
	// "not loaded yet"; the caller is responsible for never starting it.
	Disabled bool
}

// Status describes the list a Store is filtering with, for display.
type Status struct {
	// Loaded is false until a list (cached or fetched) has been accepted.
	// While false the active matcher blocks nothing.
	Loaded bool
	// Rules is the number of compiled rules in the active list.
	Rules int
	// LastFetch is when the active list was fetched: the time of the last
	// successful download, or the cache file's modification time when the
	// list came from the cache. Zero when nothing is loaded.
	LastFetch time.Time
	// LastChecked is when the last refresh attempt finished, successful or
	// not. Zero until one has run in this process.
	LastChecked time.Time
	// LastError is the most recent failure, cleared by a clean refresh.
	LastError string
	// Disabled is true when the operator turned the download off, so no list
	// will ever load (Options.Disabled).
	Disabled bool
}

// Store owns the active blocklist. It loads a cached copy at start, then
// fetches and periodically refreshes the list in the background. Readers call
// Matcher, which is a lock-free atomic load.
type Store struct {
	opts   Options
	active atomic.Pointer[Matcher]
	// baseline is the rule count of the last list this process fetched; the
	// shrink floor compares against it. It is zero after a cache load, so a
	// stale cache (old URL, upstream restructure) never blocks a fresh fetch
	// and a restart recovers a wedged store. Guarded by refreshMu.
	baseline int

	// refreshMu serializes loads (fetch through persist), so a slow earlier
	// fetch can never overwrite a newer list in memory or in the cache.
	refreshMu sync.Mutex

	// mu guards status and is held across the matcher swap, so a Status
	// snapshot always describes the matcher that was active when it was taken.
	mu      sync.Mutex
	status  Status
	checked time.Time
}

// NewStore returns a Store with an empty (block-nothing) matcher.
func NewStore(opts Options) *Store {
	if opts.Interval <= 0 {
		opts.Interval = DefaultRefreshInterval
	}
	if opts.RetryInitial <= 0 {
		opts.RetryInitial = DefaultRetryInitial
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultRetryMax
	}
	if opts.Client == nil {
		opts.Client = httpsafe.SafeClient(fetchTimeout)
	}
	if opts.URL == "" {
		opts.URL = SourceURL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Store{opts: opts}
	s.status.Disabled = opts.Disabled
	s.active.Store(emptyMatcher)
	return s
}

// snapshot returns the active matcher and its Status under mu, the lock publish
// holds across the swap, so the pair always describes one list.
func (s *Store) snapshot() (*Matcher, Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.LastChecked = s.checked
	return s.active.Load(), st
}

// Matcher returns the active matcher. It is never nil.
func (s *Store) Matcher() *Matcher { return s.active.Load() }

// Status returns a snapshot of the store's state.
func (s *Store) Status() Status {
	_, st := s.snapshot()
	return st
}

// SourceHost is the host (no path, no query) the list is fetched from, or ""
// when the download is disabled or the URL does not parse. It is safe to show.
func (s *Store) SourceHost() string {
	if s.opts.Disabled {
		return ""
	}
	u, err := url.Parse(s.opts.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// SourceDisplay returns a form of the list URL that is safe to show an
// administrator: display is host plus path (no scheme, userinfo, query or
// fragment), and repo is "owner/repo" when the URL points into GitHub
// (raw.githubusercontent.com or github.com with at least two path segments),
// else "". Both are "" when the download is disabled or the URL does not parse.
func (s *Store) SourceDisplay() (display, repo string) {
	if s.opts.Disabled {
		return "", ""
	}
	return DescribeSource(s.opts.URL)
}

// DescribeSource is the pure form of SourceDisplay.
func DescribeSource(raw string) (display, repo string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ""
	}
	display = u.Host + u.EscapedPath()
	switch strings.ToLower(u.Hostname()) {
	case "raw.githubusercontent.com", "github.com":
		// Segment the ESCAPED path so an encoded slash (%2F) stays inside one
		// segment; a segment with any escape is not a GitHub owner or repo name.
		segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
		if len(segs) >= 2 && segs[0] != "" && segs[1] != "" &&
			!strings.Contains(segs[0]+segs[1], "%") {
			return display, segs[0] + "/" + segs[1]
		}
	}
	return display, ""
}

// Start loads the cached list (a local read, so filtering works right after a
// restart), then fetches and refreshes in a background goroutine until ctx is
// canceled. The returned channel closes when that goroutine has exited.
func (s *Store) Start(ctx context.Context) <-chan struct{} {
	s.loadCache()
	done := make(chan struct{})
	go func() {
		defer close(done)
		retry := s.opts.RetryInitial
		for {
			err := s.Refresh(ctx)
			if err != nil && ctx.Err() != nil {
				return
			}
			var wait time.Duration
			wait, retry = s.nextWait(err, retry)
			if err != nil {
				msg := "AI blocklist: refresh failed; keeping the current list"
				if errors.Is(err, errCacheWrite) {
					msg = "AI blocklist: list is active in memory but could not be cached"
				}
				s.opts.Logger.Warn(msg, slog.String("url", s.opts.URL),
					slog.String("error", err.Error()), slog.Duration("next_attempt_in", wait))
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return done
}

// nextWait schedules the next refresh from the last result. Only a fetch
// failure backs off (retry, doubling, capped by RetryMax and Interval), since a
// refetch cannot fix a rejected list or an unwritable cache; everything else
// waits the normal Interval and resets the backoff. It returns the wait and
// the retry delay to use after it.
func (s *Store) nextWait(err error, retry time.Duration) (wait, next time.Duration) {
	var fe fetchErr
	if !errors.As(err, &fe) {
		return s.opts.Interval, s.opts.RetryInitial
	}
	// Saturate before doubling: retry*2 would overflow Duration for a huge
	// RetryMax and go negative, which makes time.NewTimer fire immediately.
	next = s.opts.RetryMax
	if retry <= next/2 {
		next = retry * 2
	}
	return min(retry, s.opts.RetryMax, s.opts.Interval), next
}

// loadCache adopts the cached list if there is a usable one.
func (s *Store) loadCache() {
	if s.opts.CacheDir == "" {
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	path := filepath.Join(s.opts.CacheDir, CacheFileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var body []byte
	switch {
	case err != nil:
	case info.Size() > maxListBytes:
		err = fmt.Errorf("exceeds %d bytes", maxListBytes)
	default:
		body, err = os.ReadFile(path) //nolint:gosec // G304: path is operator config plus a constant name
	}
	var m *Matcher
	if err == nil {
		m, err = s.validate(string(body))
	}
	if err == nil {
		s.publish(m, info.ModTime())
	} else {
		s.setError(fmt.Errorf("cached list: %w", err))
		s.opts.Logger.Warn("AI blocklist: ignoring cached list",
			slog.String("path", path), slog.String("error", err.Error()))
	}
}

// Refresh fetches the list once. A list is adopted only if it downloads in
// full with a 200 and passes validate's checks; on any such failure the
// current matcher and the cache file are left untouched. Once valid the list is
// written to the cache and then published; if that write fails the list is
// still published (a valid list beats none, e.g. on a read-only data dir) and
// the returned error wraps errCacheWrite. Concurrent calls serialize.
func (s *Store) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refreshLocked(ctx)
}

// RefreshIfDue runs Refresh unless an attempt finished less than minGap ago, in
// which case it returns ran=false and how long until one is allowed. The check
// happens under the refresh lock, so there is one fetch however many callers.
// A caller that finds a refresh already running does not wait for it (that can
// take the client timeout): it gets ran=false with minGap as the retry hint.
func (s *Store) RefreshIfDue(ctx context.Context, minGap time.Duration) (ran bool, retryAfter time.Duration, err error) {
	if !s.refreshMu.TryLock() {
		return false, minGap, nil
	}
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	last := s.checked
	s.mu.Unlock()
	if wait := minGap - time.Since(last); !last.IsZero() && wait > 0 {
		return false, wait, nil
	}
	return true, 0, s.refreshLocked(ctx)
}

// refreshLocked is Refresh with refreshMu already held.
func (s *Store) refreshLocked(ctx context.Context) error {
	// NewStore swaps an empty URL for SourceURL, so a disabled store would
	// otherwise download from upstream; refuse before touching the network.
	if s.opts.Disabled {
		return errors.New("list download is disabled")
	}
	defer func() {
		s.mu.Lock()
		s.checked = time.Now()
		s.mu.Unlock()
	}()
	body, err := s.fetch(ctx)
	if err != nil && !errors.Is(err, errOversize) {
		err = fetchErr{err}
	}
	var m *Matcher
	if err == nil {
		m, err = s.validate(body)
	}
	if err != nil {
		s.setError(err)
		return err
	}
	var werr error
	if s.opts.CacheDir != "" {
		path := filepath.Join(s.opts.CacheDir, CacheFileName)
		if err := filesystem.WriteFileAtomic(path, []byte(body), 0o644); err != nil {
			werr = fmt.Errorf("%w: writing cache: %w", errCacheWrite, err)
		}
	}
	s.publish(m, time.Now())
	s.baseline = m.ruleCount()
	if werr != nil {
		s.setError(werr)
	}
	return werr
}

func (s *Store) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater"))
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching list: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching list: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading list: %w", err)
	}
	if len(data) > maxListBytes {
		return "", errOversize
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

// validate parses body and returns its matcher if it is acceptable, without
// publishing it. It rejects a list with no rules, over the regex caps, blocking
// a canary, or shrinking the last fetched list below shrinkFloorPercent.
// Callers hold refreshMu, so the active list cannot change underneath it.
func (s *Store) validate(body string) (*Matcher, error) {
	if err := checkRegexCaps(body); err != nil {
		return nil, err
	}
	m := Parse(body)
	n := m.ruleCount()
	if n == 0 {
		return nil, errors.New("list has no usable rules")
	}
	for _, u := range canaryURLs {
		if m.MatchURL(u) {
			return nil, fmt.Errorf("list blocks known-clean URL %s", u)
		}
	}
	if n*100 < s.baseline*shrinkFloorPercent {
		return nil, fmt.Errorf("list has %d rules, under %d%% of the %d fetched earlier; restart Stillwater to accept it",
			n, shrinkFloorPercent, s.baseline)
	}
	if m.Skipped > 0 {
		s.opts.Logger.Warn("AI blocklist: skipped unparsable lines", slog.Int("skipped", m.Skipped))
	}
	return m, nil
}

// publish makes a validated matcher the active one.
func (s *Store) publish(m *Matcher, fetched time.Time) {
	s.mu.Lock()
	s.active.Store(m)
	s.status = Status{Loaded: true, Rules: m.ruleCount(), LastFetch: fetched, Disabled: s.opts.Disabled}
	s.mu.Unlock()
}

// checkRegexCaps rejects a list with too many or too long regex lines. It
// scans the source (the lines Parse treats as regexes) before anything is
// compiled, so an oversized list never reaches regexp.Compile.
func checkRegexCaps(body string) error {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		if len(line) > maxRegexBytes {
			return fmt.Errorf("list has a regex over %d bytes", maxRegexBytes)
		}
		count++
		if count > maxRegexes {
			return fmt.Errorf("list has over %d regexes", maxRegexes)
		}
	}
	return nil
}

func (s *Store) setError(err error) {
	s.mu.Lock()
	s.status.LastError = err.Error()
	s.mu.Unlock()
}
