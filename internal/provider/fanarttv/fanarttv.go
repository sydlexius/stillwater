package fanarttv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/httpsafe"
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
		client:   httpsafe.SafeClient(10 * time.Second),
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

// SearchArtist is a documented no-op for Fanart.tv (lookup is by MBID only).
// Injection is intentionally NOT consulted here: the production contract is
// (nil, nil), and callers that treat a nil error from a known-no-op as
// "provider does not support this" would otherwise behave differently under
// the smoke harness than in prod -- which would test the harness, not the
// silent-failure surfaces the harness exists to catch.
func (a *Adapter) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

// GetArtist is a documented no-op for Fanart.tv (images only). Injection is
// intentionally NOT consulted here; see SearchArtist for rationale.
func (a *Adapter) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}

// GetImages fetches available images for an artist by their MusicBrainz ID.
func (a *Adapter) GetImages(ctx context.Context, mbid string) ([]provider.ImageResult, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	a.logger.Debug("requesting images", slog.String("mbid", mbid))

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameFanartTV,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameFanartTV, ID: mbid}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameFanartTV,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
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
	var nf *provider.ErrNotFound
	return errors.As(err, &nf)
}

func mapImages(resp *Response) []provider.ImageResult {
	results := make([]provider.ImageResult, 0,
		len(resp.ArtistThumb)+len(resp.ArtistBackground)+
			len(resp.HDMusicLogo)+len(resp.MusicLogo)+len(resp.MusicBanner))
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
