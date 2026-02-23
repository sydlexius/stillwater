package duckduckgo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultBaseURL = "https://duckduckgo.com"
	htmlBaseURL    = "https://html.duckduckgo.com"
	maxResults     = 30
	userAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// searchTerms maps image types to query suffix templates.
// The artist name is prepended to these terms.
var searchTerms = map[provider.ImageType]string{
	provider.ImageThumb:  "artist photo portrait",
	provider.ImageFanart: "band wallpaper background high resolution",
	provider.ImageLogo:   "band logo png transparent",
	provider.ImageBanner: "band banner header wide",
}

var vqdRegex = regexp.MustCompile(`vqd=([0-9-]+)`)

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
		client:  &http.Client{Timeout: 15 * time.Second},
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
	suffix, ok := searchTerms[imageType]
	if !ok {
		return nil, nil
	}
	query := artistName + " " + suffix

	if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	vqd, err := a.getVQDToken(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("getting VQD token: %w", err)
	}

	if err := a.limiter.Wait(ctx, provider.NameDuckDuckGo); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	images, err := a.fetchImages(ctx, query, vqd)
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

// getVQDToken obtains the validation query digest token from DuckDuckGo.
func (a *Adapter) getVQDToken(ctx context.Context, query string) (string, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.htmlURL+"/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from adapter config, not user input
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD request returned status %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}

	matches := vqdRegex.FindSubmatch(body)
	if len(matches) < 2 {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("VQD token not found in response"),
		}
	}

	return string(matches[1]), nil
}

// fetchImages queries the DuckDuckGo image search JSON endpoint.
func (a *Adapter) fetchImages(ctx context.Context, query, vqd string) ([]imageHit, error) {
	params := url.Values{
		"l":   {"us-en"},
		"o":   {"json"},
		"q":   {query},
		"vqd": {vqd},
		"f":   {",,,,,"},
		"p":   {"1"},
	}

	reqURL := a.baseURL + "/i.js?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", a.baseURL+"/")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from adapter config, not user input
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDuckDuckGo,
			Cause:    fmt.Errorf("image search returned status %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}

	var searchResp imageSearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("parsing image results: %w", err)
	}

	return searchResp.Results, nil
}
