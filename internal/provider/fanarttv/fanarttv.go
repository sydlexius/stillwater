package fanarttv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const defaultBaseURL = "https://webservice.fanart.tv/v3/music"

// Adapter implements the provider.Provider interface for Fanart.tv.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings *provider.SettingsService
	logger   *slog.Logger
	baseURL  string
}

// New creates a Fanart.tv adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Fanart.tv adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 10 * time.Second},
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "fanarttv")),
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameFanartTV }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return true }

// SearchArtist is not supported by Fanart.tv (lookup by MBID only).
func (a *Adapter) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

// GetArtist is not supported by Fanart.tv (images only).
func (a *Adapter) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}

// GetImages fetches available images for an artist by their MusicBrainz ID.
func (a *Adapter) GetImages(ctx context.Context, mbid string) ([]provider.ImageResult, error) {
	apiKey, err := a.settings.GetAPIKey(ctx, provider.NameFanartTV)
	if err != nil {
		return nil, fmt.Errorf("getting API key: %w", err)
	}
	if apiKey == "" {
		return nil, &provider.ErrAuthRequired{Provider: provider.NameFanartTV}
	}

	if err := a.limiter.Wait(ctx, provider.NameFanartTV); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameFanartTV,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := fmt.Sprintf("%s/%s?api_key=%s", a.baseURL, mbid, apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	a.logger.Debug("requesting images", slog.String("mbid", mbid))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + MBID
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameFanartTV,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		return nil, &provider.ErrNotFound{Provider: provider.NameFanartTV, ID: mbid}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameFanartTV,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var fanart Response
	if err := json.Unmarshal(body, &fanart); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return mapImages(&fanart), nil
}

// TestConnection verifies the API key is valid.
func (a *Adapter) TestConnection(ctx context.Context) error {
	apiKey, err := a.settings.GetAPIKey(ctx, provider.NameFanartTV)
	if err != nil {
		return fmt.Errorf("getting API key: %w", err)
	}
	if apiKey == "" {
		return &provider.ErrAuthRequired{Provider: provider.NameFanartTV}
	}

	// Use Radiohead MBID for testing (known to have images)
	_, err = a.GetImages(ctx, "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		var notFound *provider.ErrNotFound
		_ = notFound
		// A 404 still means the API key is valid
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	_, ok := err.(*provider.ErrNotFound)
	return ok
}

func mapImages(resp *Response) []provider.ImageResult {
	var results []provider.ImageResult
	source := string(provider.NameFanartTV)

	for _, img := range resp.ArtistThumb {
		results = append(results, provider.ImageResult{
			URL:      img.URL,
			Type:     provider.ImageThumb,
			Likes:    parseLikes(img.Likes),
			Language: img.Lang,
			Source:   source,
		})
	}
	for _, img := range resp.ArtistBackground {
		results = append(results, provider.ImageResult{
			URL:      img.URL,
			Type:     provider.ImageFanart,
			Likes:    parseLikes(img.Likes),
			Language: img.Lang,
			Source:   source,
		})
	}
	for _, img := range resp.HDMusicLogo {
		results = append(results, provider.ImageResult{
			URL:      img.URL,
			Type:     provider.ImageHDLogo,
			Likes:    parseLikes(img.Likes),
			Language: img.Lang,
			Source:   source,
		})
	}
	for _, img := range resp.MusicLogo {
		results = append(results, provider.ImageResult{
			URL:      img.URL,
			Type:     provider.ImageLogo,
			Likes:    parseLikes(img.Likes),
			Language: img.Lang,
			Source:   source,
		})
	}
	for _, img := range resp.MusicBanner {
		results = append(results, provider.ImageResult{
			URL:      img.URL,
			Type:     provider.ImageBanner,
			Likes:    parseLikes(img.Likes),
			Language: img.Lang,
			Source:   source,
		})
	}

	return results
}

func parseLikes(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
