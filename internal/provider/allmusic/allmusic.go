package allmusic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultBaseURL = "https://www.allmusic.com"
	// maxResponseBytes caps the HTML response body to prevent unbounded memory usage.
	maxResponseBytes = 2 * 1024 * 1024 // 2 MB
	userAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// ErrScraperBroken indicates that the expected HTML structure was not found.
// This typically means AllMusic has changed their page layout and the scraper
// needs to be updated.
var ErrScraperBroken = errors.New("allmusic: expected HTML structure not found")

// Adapter implements provider.WebMetadataScraper for AllMusic.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	baseURL string
}

// New creates an AllMusic scraper adapter.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL)
}

// NewWithBaseURL creates an AllMusic adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL string) *Adapter {
	if limiter == nil {
		panic("allmusic: limiter must not be nil")
	}
	return &Adapter{
		client:  &http.Client{Timeout: 15 * time.Second},
		limiter: limiter,
		logger:  logger.With(slog.String("provider", "allmusic")),
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider identifier.
func (a *Adapter) Name() provider.ProviderName { return provider.NameAllMusic }

// RequiresAuth returns false since AllMusic needs no API key.
func (a *Adapter) RequiresAuth() bool { return false }

// ScrapeArtist fetches an AllMusic artist page by ID and extracts genre/style data.
// The id parameter should be an AllMusic artist ID (e.g. "mn0000505828").
func (a *Adapter) ScrapeArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if id == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameAllMusic, ID: id}
	}

	if err := a.limiter.Wait(ctx, provider.NameAllMusic); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAllMusic,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := a.baseURL + "/artist/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from adapter config, not user input
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAllMusic,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameAllMusic, ID: id}
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAllMusic,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if int64(len(body)) > maxResponseBytes {
		a.logger.Warn("AllMusic response truncated at size limit",
			slog.String("id", id),
			slog.Int64("max_bytes", maxResponseBytes))
		body = body[:maxResponseBytes]
	}

	genres, styles, err := parseArtistPage(body)
	if err != nil {
		return nil, err
	}

	meta := &provider.ArtistMetadata{
		ProviderID: id,
		AllMusicID: id,
		Genres:     genres,
		Styles:     styles,
		Moods:      []string{}, // AllMusic loads moods via AJAX; endpoint unknown
	}

	a.logger.Debug("scrape completed",
		slog.String("id", id),
		slog.Int("genres", len(genres)),
		slog.Int("styles", len(styles)))

	return meta, nil
}

// parseArtistPage parses AllMusic artist HTML and extracts genres and styles.
// Returns ErrScraperBroken if the expected HTML structure is not found at all.
func parseArtistPage(body []byte) (genres, styles []string, err error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing HTML: %w", err)
	}

	genres = extractSection(doc, "artist-genres")
	styles = extractSection(doc, "artist-styles")

	// If neither section is found, the page structure has likely changed.
	// Return the sentinel error so callers can detect and report it.
	// We do not check for other page content (e.g. h1) because AllMusic
	// could change their markup to remove genre/style divs while keeping
	// other elements, which would cause the scraper to silently return
	// empty data instead of signaling that an update is needed.
	if genres == nil && styles == nil {
		return nil, nil, ErrScraperBroken
	}

	// Normalize nil slices to empty slices for consistent JSON output.
	if genres == nil {
		genres = []string{}
	}
	if styles == nil {
		styles = []string{}
	}

	return genres, styles, nil
}

// extractSection finds a div with the given class name and extracts all
// anchor text from within it. AllMusic uses a structure like:
//
//	<div class="artist-genres">
//	  <h4>Genre</h4>
//	  <div><a href="...">Country</a></div>
//	  <div><a href="...">Pop</a></div>
//	</div>
func extractSection(doc *html.Node, className string) []string {
	container := findNodeByClass(doc, className)
	if container == nil {
		return nil
	}

	// Return an empty (non-nil) slice when the section exists but has
	// no anchor elements. This distinguishes "section present, no data"
	// from "section not found" (nil), which triggers ErrScraperBroken.
	values := []string{}
	collectAnchorText(container, &values)
	return values
}

// findNodeByClass performs a depth-first search for an element node whose
// "class" attribute contains the target class name.
func findNodeByClass(n *html.Node, className string) *html.Node {
	if n.Type == html.ElementNode {
		for _, attr := range n.Attr {
			if attr.Key == "class" && containsClass(attr.Val, className) {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNodeByClass(c, className); found != nil {
			return found
		}
	}
	return nil
}

// containsClass checks whether a space-separated class attribute value
// contains the target class name as a whole word.
func containsClass(attrVal, className string) bool {
	for _, cls := range strings.Fields(attrVal) {
		if cls == className {
			return true
		}
	}
	return false
}

// collectAnchorText walks the subtree and collects trimmed text content
// from all <a> elements.
func collectAnchorText(n *html.Node, values *[]string) {
	if n.Type == html.ElementNode && n.Data == "a" {
		text := extractText(n)
		text = strings.TrimSpace(text)
		if text != "" {
			*values = append(*values, text)
		}
		return // do not recurse into nested anchors
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectAnchorText(c, values)
	}
}

// extractText concatenates all text nodes under the given node.
func extractText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(extractText(c))
	}
	return sb.String()
}
