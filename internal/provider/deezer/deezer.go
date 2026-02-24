package deezer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sydlexius/stillwater/internal/provider"
)

const defaultBaseURL = "https://api.deezer.com"

// Adapter implements provider.Provider for Deezer's public API.
// No authentication is required. Deezer provides artist search and
// high-quality artist thumbnail images.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	baseURL string
}

// New creates a Deezer adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Deezer adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:  &http.Client{Timeout: 10 * time.Second},
		limiter: limiter,
		logger:  logger.With(slog.String("provider", "deezer")),
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider identifier.
func (a *Adapter) Name() provider.ProviderName { return provider.NameDeezer }

// RequiresAuth returns false since Deezer's public API needs no API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist searches Deezer for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if name == "" {
		return nil, nil
	}

	if err := a.limiter.Wait(ctx, provider.NameDeezer); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{
		"q":     {name},
		"limit": {"10"},
	}
	reqURL := a.baseURL + "/search/artist?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Data))
	for _, r := range resp.Data {
		results = append(results, provider.ArtistSearchResult{
			ProviderID: strconv.Itoa(r.ID),
			Name:       r.Name,
			Score:      100,
			Source:     string(provider.NameDeezer),
		})
	}

	a.logger.Debug("artist search completed",
		slog.String("query", name),
		slog.Int("results", len(results)))

	return results, nil
}

// GetArtist fetches metadata for an artist by their Deezer ID (numeric string).
// Returns ErrNotFound for non-numeric IDs such as MusicBrainz UUIDs, since
// Deezer does not index by MBID.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if !isDeezerID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: id}
	}

	if err := a.limiter.Wait(ctx, provider.NameDeezer); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := fmt.Sprintf("%s/artist/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var result artistResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	meta := &provider.ArtistMetadata{
		ProviderID: strconv.Itoa(result.ID),
		Name:       result.Name,
	}
	if result.Link != "" {
		meta.URLs = map[string]string{"deezer": result.Link}
	}

	return meta, nil
}

// GetImages fetches artist thumbnail images by Deezer ID.
// Returns ErrNotFound for non-numeric IDs such as MusicBrainz UUIDs.
func (a *Adapter) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if !isDeezerID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: id}
	}

	if err := a.limiter.Wait(ctx, provider.NameDeezer); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := fmt.Sprintf("%s/artist/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var result artistResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	return imagesFromResult(&result), nil
}

// doRequest executes a GET request and returns the response body.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from adapter config and validated inputs
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		// continue
	case http.StatusNotFound:
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: reqURL}
	case http.StatusTooManyRequests:
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    fmt.Errorf("rate limited by server"),
		}
	default:
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    fmt.Errorf("unexpected status %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
}

// isDeezerID reports whether id is a valid Deezer artist ID (all digits).
func isDeezerID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// imagesFromResult converts a Deezer artist result into ImageResult values.
// Deezer provides artist photos at multiple sizes; the XL variant is preferred.
func imagesFromResult(r *artistResult) []provider.ImageResult {
	source := string(provider.NameDeezer)
	var images []provider.ImageResult

	if r.PictureXL != "" && !isDefaultPicture(r.PictureXL) {
		images = append(images, provider.ImageResult{
			URL:    r.PictureXL,
			Type:   provider.ImageThumb,
			Source: source,
		})
	} else if r.PictureBig != "" && !isDefaultPicture(r.PictureBig) {
		images = append(images, provider.ImageResult{
			URL:    r.PictureBig,
			Type:   provider.ImageThumb,
			Source: source,
		})
	}

	return images
}

// isDefaultPicture reports whether a Deezer picture URL is the generic placeholder.
// Deezer returns URLs containing "/images/artist//" (double slash) for artists
// without a photo.
func isDefaultPicture(u string) bool {
	return strings.Contains(u, "/images/artist//")
}
