package duckduckgo

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultBaseURL = "https://duckduckgo.com"
	htmlBaseURL    = "https://html.duckduckgo.com"
	maxResults     = 30
	// maxExtraPages caps how many additional i.js pages a filtered search may
	// follow (#3309). The first page is always fetched; paging happens only
	// when the caller's filter left fewer than maxResults survivors.
	maxExtraPages    = 2
	maxArtistNameLen = 200
	userAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// acceptLanguage is sent on every request to DuckDuckGo. Measured
	// 2026-09-28 (issue #3228 comment 5880827938): a request without it gets
	// flagged as bot traffic, and the flag then penalizes the whole IP for a
	// few minutes -- not just the one request. Every DDG request in this
	// adapter, including the HTML-fallback vqd fetch, sets this header.
	acceptLanguage = "en-US,en;q=0.9"
)

// searchTerms maps image types to query suffix templates.
// The artist name is prepended to these terms.
var searchTerms = map[provider.ImageType]string{
	provider.ImageThumb:  "artist photo portrait",
	provider.ImageFanart: "band wallpaper background high resolution",
	provider.ImageLogo:   "band logo png transparent",
	provider.ImageBanner: "band banner header wide",
}

// vqdRegex matches quoted VQD tokens as DuckDuckGo's MAIN search page
// actually emits them (confirmed live 2026-09-28 during hostile review):
//   - vqd='4-123456789' (single-quoted, in a script tag)
//   - vqd="4-123456789" (double-quoted, in a script tag)
//
// Go's regexp package has no backreferences, so the quote-matching pair from
// the issue (`vqd=(['"])(\d[\d-]+)\1`) is expressed as two alternations, one
// per quote style; extractVQDFromBytes picks whichever capture group is
// non-empty. This intentionally no longer matches an unquoted
// "vqd=4-123456789" query-parameter-style token: the real MAIN page only
// emits the quoted form, and the looser previous regex (accepting bare
// alphanumerics with no digit requirement) was more permissive than
// anything DDG has been observed to send from that page.
//
// This regex is scoped to getVQDFromMainPage ONLY. The legacy /html/
// fallback endpoint (getVQDFromHTMLPage) was never live-verified against
// the quoted-only claim above -- its unquoted fixture predates #3273 and
// was never re-measured against the real endpoint -- so it uses the
// separate, looser vqdFallbackRegex below instead of this one. Widening
// this shared regex to also accept the fallback's unquoted style would let
// a main-page response silently match that style too, which the live
// measurement never observed; keeping two regexes keeps each one no looser
// than what was actually verified for its own call site.
var vqdRegex = regexp.MustCompile(`vqd='(\d[\d-]+)'|vqd="(\d[\d-]+)"`)

// vqdFallbackRegex matches VQD tokens on the legacy /html/ fallback
// endpoint (getVQDFromHTMLPage). Unlike vqdRegex, this accepts BOTH the
// quoted form (in case the fallback ever emits it) and the unquoted
// digits-dash query-parameter style historically observed there (e.g.
// "vqd=98765&"), because the fallback's exact current format was never
// live-verified during #3273 -- only the main page was. The unquoted
// alternative requires the digit run to be followed by a non-digit,
// non-dash character (or end of input) so it does not partially match
// inside a longer numeric token.
var vqdFallbackRegex = regexp.MustCompile(`vqd='(\d[\d-]+)'|vqd="(\d[\d-]+)"|vqd=(\d[\d-]+)(?:[^\d-]|$)`)

// Adapter implements provider.WebImageProvider for DuckDuckGo image search.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	baseURL string
	htmlURL string

	// noVQDFallback skips the /html/ POST fallback in getVQDToken. Only the
	// live canary sets it (#3230), so it makes exactly one search's requests
	// and reports the real first-stage error. Production leaves it false.
	noVQDFallback bool
}

// New creates a DuckDuckGo image search adapter with default URLs.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL, htmlBaseURL)
}

// NewWithBaseURL creates a DuckDuckGo adapter with custom base URLs (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL, htmlURL string) *Adapter {
	return &Adapter{
		client:  httpsafe.SafeClient(15 * time.Second),
		limiter: limiter,
		logger:  logger.With(slog.String("provider", "duckduckgo")),
		baseURL: strings.TrimRight(baseURL, "/"),
		htmlURL: strings.TrimRight(htmlURL, "/"),
	}
}

// Name returns the provider identifier.
func (a *Adapter) Name() provider.ProviderName { return provider.NameDuckDuckGo }

// RequiresAuth returns false since DuckDuckGo needs no API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchImages queries DuckDuckGo image search for artist images of a specific type.
func (a *Adapter) SearchImages(ctx context.Context, artistName string, imageType provider.ImageType) ([]provider.ImageResult, error) {
	results, _, err := a.searchImages(ctx, artistName, imageType, nil)
	return results, err
}

// SearchImagesFiltered is SearchImages with a caller-supplied filter applied
// to each fetched page BEFORE the maxResults cap (#3309), so results the
// filter drops do not use up cap slots. The first page is filtered whole and
// capped afterwards; only when fewer than maxResults survive does the adapter
// follow the response's `next` cursor, at most maxExtraPages more times. It
// returns the survivors and the total number the filter removed across every
// page it processed. Extra pages reuse the first request's headers, cookie
// jar and vqd token and go through the rate limiter; a failed extra page
// (including a 403/202 challenge) is never retried and the search returns what
// it already has.
func (a *Adapter) SearchImagesFiltered(ctx context.Context, artistName string, imageType provider.ImageType, keep provider.ImageFilter) ([]provider.ImageResult, int, error) {
	return a.searchImages(ctx, artistName, imageType, keep)
}

func (a *Adapter) searchImages(ctx context.Context, artistName string, imageType provider.ImageType, keep provider.ImageFilter) ([]provider.ImageResult, int, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, 0, provider.ErrInjectedFailure
	}
	if artistName == "" || len(artistName) > maxArtistNameLen {
		return nil, 0, nil
	}

	suffix, ok := searchTerms[imageType]
	if !ok {
		return nil, 0, nil
	}
	query := artistName + " " + suffix

	if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
		return nil, 0, fmt.Errorf("rate limiter: %w", err)
	}

	// A fresh cookie jar per search: the vqd fetch and the i.js fetch(es) for
	// THIS search share it (DDG's i.js request expects the cookie the vqd
	// response set), but nothing carries over to the next search. The
	// Transport (and its httpsafe SSRF guard) is shared with the long-lived
	// a.client -- only the Jar is new -- so this is not a bare http.Client.
	client := a.newSearchClient()

	vqd, err := a.getVQDToken(ctx, client, query)
	if err != nil {
		return nil, 0, fmt.Errorf("getting VQD token: %w", err)
	}

	var (
		results []provider.ImageResult
		removed int
		offset  int
	)
	for page := 0; ; page++ {
		if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
			if page == 0 || ctx.Err() != nil {
				return nil, 0, fmt.Errorf("rate limiter: %w", err)
			}
			break
		}

		resp, err := a.fetchImages(ctx, client, query, vqd, offset)
		if err != nil {
			if page == 0 || ctx.Err() != nil {
				// A canceled request surfaces its error so the caller stops
				// instead of rendering a partial grid. Only a provider-side
				// failure on an extra page degrades to partial results.
				return nil, 0, fmt.Errorf("fetching images: %w", err)
			}
			// An extra page failed (e.g. a 403/202 challenge): never retry,
			// keep what the earlier pages produced.
			a.logger.Warn("extra image page failed, returning partial results",
				slog.Int("page", page), slog.Any("error", err))
			break
		}

		pageResults := hitsToResults(resp.Results, imageType)
		if keep != nil {
			var n int
			pageResults, n = keep(pageResults)
			removed += n
		}
		results = append(results, pageResults...)
		if len(results) >= maxResults {
			results = results[:maxResults]
			break
		}

		next, ok := nextOffset(resp.Next, offset)
		if !ok || keep == nil || page >= maxExtraPages {
			break
		}
		offset = next
	}

	a.logger.Debug("image search completed",
		slog.String("artist", artistName),
		slog.String("type", string(imageType)),
		slog.Int("results", len(results)))

	return results, removed, nil
}

// hitsToResults converts raw hits to ImageResults, dropping hits with no image URL.
func hitsToResults(hits []imageHit, imageType provider.ImageType) []provider.ImageResult {
	var out []provider.ImageResult
	for _, hit := range hits {
		if hit.Image == "" {
			continue
		}
		out = append(out, provider.ImageResult{
			URL:    hit.Image,
			Type:   imageType,
			Width:  hit.Width,
			Height: hit.Height,
			Source: string(provider.NameDuckDuckGo),
		})
	}
	return out
}

// nextOffset extracts the result offset (the `s` param) from a response's
// `next` cursor. Only the numeric offset is taken; the rest of the next
// request is rebuilt from the first request's own params, so a hostile or
// malformed cursor cannot redirect the request or change its shape. It
// reports false when there is no usable cursor or the offset does not advance.
func nextOffset(next string, prev int) (int, bool) {
	if next == "" {
		return 0, false
	}
	u, err := url.Parse(next)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(u.Query().Get("s"))
	if err != nil || n <= prev {
		return 0, false
	}
	return n, true
}

// newSearchClient returns an *http.Client for one SearchImages call: same
// Transport (and Timeout) as the adapter's long-lived a.client, so the
// httpsafe SSRF guard on that Transport still applies, but with a brand-new
// cookiejar.Jar. The vqd fetch and the i.js fetch for this one search share
// the jar (DDG's i.js request expects the cookie set by the vqd response);
// nothing is shared with any other search. cookiejar.New never returns a
// non-nil error for a nil Options argument (see the stdlib source: it only
// validates the PublicSuffixList, which is nil here), so the error is
// intentionally discarded.
func (a *Adapter) newSearchClient() *http.Client {
	// cookiejar.New only ever returns a non-nil error when validating a
	// supplied PublicSuffixList; passing nil skips that entirely, so this
	// call cannot fail.
	jar, _ := cookiejar.New(nil)
	return httpsafe.ClientWithJar(a.client, jar)
}

// vqdRequestURL builds the vqd-fetch URL for a query. It is also reused as
// the Referer on the i.js request: DDG's known-good request (issue #3228
// comment 5880827938) sends the full images-search URL as the i.js Referer,
// and this is that URL regardless of which of the two vqd-fetch paths
// (main page or HTML fallback) actually produced the token.
func vqdRequestURL(baseURL, query string) string {
	params := url.Values{"q": {query}, "iax": {"images"}, "ia": {"images"}}
	return baseURL + "/?" + params.Encode()
}

// readBody reads an HTTP response body, decoding gzip content manually when
// present. Setting Accept-Encoding ourselves (required so DDG sees the exact
// known-good header set) disables net/http's transparent decompression, so a
// Content-Encoding: gzip response must be unwrapped here. limit bounds the
// DECOMPRESSED size read, same protection the previous plain io.LimitReader
// gave against an oversized body -- applied after gzip so it still caps
// memory use on a hostile/oversized upstream response.
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	if !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		return io.ReadAll(io.LimitReader(resp.Body, limit))
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close() //nolint:errcheck // Close error not actionable on read-only decompressor

	data, err := io.ReadAll(io.LimitReader(gz, limit))
	if err != nil {
		// A raw io.ReadAll error here (e.g. gzip.ErrChecksum, or an
		// io.ErrUnexpectedEOF from truncated compressed data) has no
		// indication it came from decompression rather than the network
		// read itself, which would otherwise look identical to a plain
		// body-read failure. %w preserves errors.Is/As on the underlying
		// error (e.g. errors.Is(err, io.ErrUnexpectedEOF)).
		return nil, fmt.Errorf("decompressing gzip body: %w", err)
	}
	return data, nil
}

// getVQDToken obtains the validation query digest token from DuckDuckGo.
// It first tries the main search page (GET /?q=QUERY), which embeds the VQD
// token in a script tag or inline JS. Falls back to the HTML endpoint
// (POST /html/) for compatibility with older DDG response formats.
func (a *Adapter) getVQDToken(ctx context.Context, client *http.Client, query string) (string, error) {
	// Try the main search page first (current DDG format)
	token, err := a.getVQDFromMainPage(ctx, client, query)
	if err == nil && token != "" {
		return token, nil
	}

	if a.noVQDFallback {
		return "", err
	}

	a.logger.Debug("main page VQD extraction failed, trying HTML endpoint",
		slog.String("query", query),
		slog.Any("error", err))

	// Fall back to HTML endpoint (older DDG format)
	return a.getVQDFromHTMLPage(ctx, client, query)
}

// getVQDFromMainPage extracts the VQD token from the main DuckDuckGo search page.
func (a *Adapter) getVQDFromMainPage(ctx context.Context, client *http.Client, query string) (string, error) {
	reqURL := vqdRequestURL(a.baseURL, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD request returned status %d", resp.StatusCode),
		}
	}

	data, err := readBody(resp, 512*1024)
	if err != nil {
		return "", err
	}
	return extractVQDFromBytes(data)
}

// getVQDFromHTMLPage extracts the VQD token from the legacy HTML search
// endpoint. It also sends Accept-Language (ASSUMPTION: the issue's measured
// known-good request only covers the main page and i.js, not this fallback;
// treating this as risking the same IP penalty for the same reason -- any
// request to DDG missing this header gets flagged -- is the safer default,
// so it is applied uniformly to every request this adapter sends). It does
// not set Accept-Encoding: the measurement did not cover this path either,
// and leaving it unset keeps net/http's transparent decompression, which is
// strictly simpler when there is no evidence this path needs the manual
// header.
func (a *Adapter) getVQDFromHTMLPage(ctx context.Context, client *http.Client, query string) (string, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.htmlURL+"/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", acceptLanguage)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD request returned status %d", resp.StatusCode),
		}
	}

	data, err := readBody(resp, 512*1024)
	if err != nil {
		return "", err
	}
	return extractVQDFromHTMLFallback(data)
}

// extractVQDFromBytes extracts the VQD token from a main-page response body
// using vqdRegex (quoted forms only). The regex has one non-empty capture
// group per quote style (single vs. double); whichever one matched is the
// token.
func extractVQDFromBytes(data []byte) (string, error) {
	matches := vqdRegex.FindSubmatch(data)
	if matches == nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD token not found in response"),
		}
	}
	if len(matches[1]) > 0 {
		return string(matches[1]), nil
	}
	if len(matches[2]) > 0 {
		return string(matches[2]), nil
	}
	return "", &provider.ErrProviderUnavailable{
		Provider: provider.NameDuckDuckGo,
		Cause:    fmt.Errorf("VQD token not found in response"),
	}
}

// extractVQDFromHTMLFallback extracts the VQD token from a /html/-fallback
// response body using vqdFallbackRegex, which accepts both the quoted forms
// and the unquoted digits-dash style (see vqdFallbackRegex's doc comment for
// why this call site is looser than extractVQDFromBytes). The regex has one
// non-empty capture group per style; whichever one matched is the token.
func extractVQDFromHTMLFallback(data []byte) (string, error) {
	matches := vqdFallbackRegex.FindSubmatch(data)
	if matches == nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD token not found in response"),
		}
	}
	for _, m := range matches[1:] {
		if len(m) > 0 {
			return string(m), nil
		}
	}
	return "", &provider.ErrProviderUnavailable{
		Provider: provider.NameDuckDuckGo,
		Cause:    fmt.Errorf("VQD token not found in response"),
	}
}

// fetchImages queries the DuckDuckGo image search JSON endpoint using the
// known-good request shape measured in issue #3228 comment 5880827938: params
// q, o=json, p=1, s=0, f=",,,," (four commas), l=us-en, vqd; headers Accept,
// Accept-Language, Accept-Encoding: gzip, Referer set to the full vqd-fetch
// URL, and X-Requested-With: XMLHttpRequest. offset is the s param: 0 for the
// first page, the cursor offset for a follow-up page (#3309).
func (a *Adapter) fetchImages(ctx context.Context, client *http.Client, query, vqd string, offset int) (*imageSearchResponse, error) {
	params := url.Values{
		"q":   {query},
		"o":   {"json"},
		"p":   {"1"},
		"s":   {strconv.Itoa(offset)},
		"f":   {",,,,"},
		"l":   {"us-en"},
		"vqd": {vqd},
	}

	reqURL := a.baseURL + "/i.js?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Referer", vqdRequestURL(a.baseURL, query))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("image search returned status %d", resp.StatusCode),
		}
	}

	body, err := readBody(resp, 2*1024*1024)
	if err != nil {
		return nil, err
	}

	var searchResp imageSearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("parsing image results: %w", err)
	}

	return &searchResp, nil
}
