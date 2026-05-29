package deezer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sydlexius/stillwater/internal/httpsafe"
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
		client:  httpsafe.SafeClient(10 * time.Second),
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
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if name == "" {
		return nil, nil
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
	for i := range resp.Data {
		r := &resp.Data[i]
		results = append(results, provider.ArtistSearchResult{
			ProviderID: strconv.Itoa(r.ID),
			Name:       r.Name,
			Score:      provider.NameSimilarity(name, r.Name),
			Source:     string(provider.NameDeezer),
		})
	}

	// Sort by score descending so the best match appears first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	a.logger.Debug("artist search completed",
		slog.String("query", name),
		slog.Int("results", len(results)))

	return results, nil
}

// GetArtist fetches metadata for an artist by their Deezer ID (numeric string).
// Returns ErrNotFound for non-numeric IDs such as MusicBrainz UUIDs, since
// Deezer does not index by MBID.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if !isDeezerID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: id}
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
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if !isDeezerID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: id}
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

// doRequest executes a GET request and returns the response body, backing off
// and retrying on a rate-limited (429) or unavailable (503) response via
// provider.DoWithRetry.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	// do performs one HTTP attempt. The limiter wait lives inside it so each
	// retry triggered by DoWithRetry still respects the per-provider budget.
	do := func(ctx context.Context) (*http.Response, error) {
		if err := a.limiter.Wait(ctx, provider.NameDeezer); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameDeezer,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		return a.client.Do(req)
	}

	// DoWithRetry consumes 429/503, so the switch below only sees 200/404/other.
	resp, err := provider.DoWithRetry(ctx, provider.SystemClock(), provider.NameDeezer, provider.DefaultRetryPolicy(), do)
	if err != nil {
		var unavailable *provider.ErrProviderUnavailable
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDeezer,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	switch resp.StatusCode {
	case http.StatusOK:
		// continue
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameDeezer, ID: reqURL}
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
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
