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
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultBaseURL   = "https://duckduckgo.com"
	htmlBaseURL      = "https://html.duckduckgo.com"
	maxResults       = 30
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

// vqdRegex matches quoted VQD tokens as DuckDuckGo actually emits them:
//   - vqd='4-123456789' (single-quoted, in a script tag)
//   - vqd="4-123456789" (double-quoted, in a script tag)
//
// Go's regexp package has no backreferences, so the quote-matching pair from
// the issue (`vqd=(['"])(\d[\d-]+)\1`) is expressed as two alternations, one
// per quote style; extractVQDFromBytes picks whichever capture group is
// non-empty. This intentionally no longer matches an unquoted
// "vqd=4-123456789" query-parameter-style token: the real page only emits
// the quoted form (confirmed 2026-09-28), and the looser previous regex
// (accepting bare alphanumerics with no digit requirement) was more permissive
// than anything DDG has been observed to send.
var vqdRegex = regexp.MustCompile(`vqd='(\d[\d-]+)'|vqd="(\d[\d-]+)"`)

// Adapter implements provider.WebImageProvider for DuckDuckGo image search.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	baseURL string
	htmlURL string
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
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if artistName == "" || len(artistName) > maxArtistNameLen {
		return nil, nil
	}

	suffix, ok := searchTerms[imageType]
	if !ok {
		return nil, nil
	}
	query := artistName + " " + suffix

	if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	// A fresh cookie jar per search: the vqd fetch and the i.js fetch for
	// THIS search share it (DDG's i.js request expects the cookie the vqd
	// response set), but nothing carries over to the next search. The
	// Transport (and its httpsafe SSRF guard) is shared with the long-lived
	// a.client -- only the Jar is new -- so this is not a bare http.Client.
	client := a.newSearchClient()

	vqd, err := a.getVQDToken(ctx, client, query)
	if err != nil {
		return nil, fmt.Errorf("getting VQD token: %w", err)
	}

	if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	images, err := a.fetchImages(ctx, client, query, vqd)
	if err != nil {
		return nil, fmt.Errorf("fetching images: %w", err)
	}

	var results []provider.ImageResult
	for _, hit := range images {
		if hit.Image == "" {
			continue
		}
		results = append(results, provider.ImageResult{
			URL:    hit.Image,
			Type:   imageType,
			Width:  hit.Width,
			Height: hit.Height,
			Source: string(provider.NameDuckDuckGo),
		})
		if len(results) >= maxResults {
			break
		}
	}

	a.logger.Debug("image search completed",
		slog.String("artist", artistName),
		slog.String("type", string(imageType)),
		slog.Int("results", len(results)))

	return results, nil
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
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close() //nolint:errcheck // Close error not actionable on read-only decompressor
		reader = gz
	}
	return io.ReadAll(io.LimitReader(reader, limit))
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
	return extractVQDFromBytes(data)
}

// extractVQDFromBytes extracts the VQD token from a response body using
// vqdRegex. The regex has one non-empty capture group per quote style
// (single vs. double); whichever one matched is the token.
func extractVQDFromBytes(data []byte) (string, error) {
	matches := vqdRegex.FindSubmatch(data)
	if matches == nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD token not found in response"),
		}
	}
	if len(matches) > 1 && len(matches[1]) > 0 {
		return string(matches[1]), nil
	}
	if len(matches) > 2 && len(matches[2]) > 0 {
		return string(matches[2]), nil
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
// URL, and X-Requested-With: XMLHttpRequest.
func (a *Adapter) fetchImages(ctx context.Context, client *http.Client, query, vqd string) ([]imageHit, error) {
	params := url.Values{
		"q":   {query},
		"o":   {"json"},
		"p":   {"1"},
		"s":   {"0"},
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

	return searchResp.Results, nil
}
