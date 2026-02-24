package musicbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
)

const defaultBaseURL = "https://musicbrainz.org/ws/2"

// Adapter implements the provider.Provider interface for MusicBrainz.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	baseURL string
}

// New creates a MusicBrainz adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL)
}

// NewWithBaseURL creates a MusicBrainz adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter: limiter,
		logger:  logger.With(slog.String("provider", "musicbrainz")),
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameMusicBrainz }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist searches MusicBrainz for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	params := url.Values{
		"query": {name},
		"fmt":   {"json"},
		"limit": {"25"},
	}
	reqURL := a.baseURL + "/artist?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Artists))
	for _, a := range resp.Artists {
		results = append(results, provider.ArtistSearchResult{
			ProviderID:     a.ID,
			Name:           a.Name,
			SortName:       a.SortName,
			Type:           a.Type,
			Disambiguation: a.Disambiguation,
			Country:        a.Country,
			Score:          a.Score,
			MusicBrainzID:  a.ID,
			Source:         string(provider.NameMusicBrainz),
		})
	}
	return results, nil
}

// GetArtist fetches full metadata for an artist by their MusicBrainz ID.
func (a *Adapter) GetArtist(ctx context.Context, mbid string) (*provider.ArtistMetadata, error) {
	params := url.Values{
		"inc": {"aliases+genres+tags+ratings+url-rels+artist-rels"},
		"fmt": {"json"},
	}
	reqURL := a.baseURL + "/artist/" + url.PathEscape(mbid) + "?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var artist MBArtist
	if err := json.Unmarshal(body, &artist); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	return a.mapArtist(&artist), nil
}

// GetImages returns nil since MusicBrainz does not host artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies connectivity to the MusicBrainz API.
func (a *Adapter) TestConnection(ctx context.Context) error {
	params := url.Values{
		"query": {"test"},
		"fmt":   {"json"},
		"limit": {"1"},
	}
	reqURL := a.baseURL + "/artist?" + params.Encode()
	_, err := a.doRequest(ctx, reqURL)
	return err
}

// doRequest executes an HTTP GET with rate limiting and standard headers.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	if err := a.limiter.Wait(ctx, provider.NameMusicBrainz); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + user-provided MBID
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{
			Provider: provider.NameMusicBrainz,
			ID:       reqURL,
		}
	}

	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider:   provider.NameMusicBrainz,
			Cause:      fmt.Errorf("HTTP %d", resp.StatusCode),
			RetryAfter: 2 * time.Second,
		}
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    fmt.Errorf("unexpected HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}

// mapArtist converts a MusicBrainz artist to the common ArtistMetadata type.
func (a *Adapter) mapArtist(mb *MBArtist) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID:     mb.ID,
		MusicBrainzID:  mb.ID,
		Name:           mb.Name,
		SortName:       mb.SortName,
		Type:           mapArtistType(mb.Type),
		Gender:         strings.ToLower(mb.Gender),
		Disambiguation: mb.Disambiguation,
		Country:        mb.Country,
		URLs:           make(map[string]string),
	}

	// Life span
	if mb.LifeSpan.Begin != "" {
		if mb.Type == "Group" || mb.Type == "Orchestra" || mb.Type == "Choir" {
			meta.Formed = mb.LifeSpan.Begin
		} else {
			meta.Born = mb.LifeSpan.Begin
		}
	}
	if mb.LifeSpan.End != "" {
		if mb.Type == "Group" || mb.Type == "Orchestra" || mb.Type == "Choir" {
			meta.Disbanded = mb.LifeSpan.End
		} else {
			meta.Died = mb.LifeSpan.End
		}
	}

	// Genres from the genres array
	for _, g := range mb.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}
	// Fall back to tags if no genres
	if len(meta.Genres) == 0 {
		for _, t := range mb.Tags {
			if t.Name != "" && t.Count > 0 {
				meta.Genres = append(meta.Genres, t.Name)
			}
		}
	}

	// Aliases
	for _, alias := range mb.Aliases {
		if alias.Name != "" && alias.Name != mb.Name {
			meta.Aliases = append(meta.Aliases, alias.Name)
		}
	}

	// Relations: extract members and URLs
	for _, rel := range mb.Relations {
		switch {
		case rel.Type == "member of band" && rel.Artist != nil && rel.Direction == "backward":
			member := provider.MemberInfo{
				Name:       rel.Artist.Name,
				MBID:       rel.Artist.ID,
				IsActive:   !rel.Ended,
				DateJoined: rel.Begin,
				DateLeft:   rel.End,
			}
			member.Instruments = append(member.Instruments, rel.Attributes...)
			meta.Members = append(meta.Members, member)
		case rel.URL != nil && rel.URL.Resource != "":
			urlType := mapURLType(rel.Type)
			if urlType != "" {
				meta.URLs[urlType] = rel.URL.Resource
			}
		}
	}

	return meta
}

// mapArtistType normalizes MusicBrainz type strings.
func mapArtistType(mbType string) string {
	switch mbType {
	case "Person":
		return "solo"
	case "Group":
		return "group"
	case "Orchestra":
		return "orchestra"
	case "Choir":
		return "choir"
	case "Character":
		return "character"
	default:
		return strings.ToLower(mbType)
	}
}

// mapURLType maps a MusicBrainz URL relation type to a simple key.
func mapURLType(relType string) string {
	switch relType {
	case "official homepage":
		return "official"
	case "wikipedia":
		return "wikipedia"
	case "wikidata":
		return "wikidata"
	case "bandcamp":
		return "bandcamp"
	case "discogs":
		return "discogs"
	case "last.fm":
		return "lastfm"
	case "allmusic":
		return "allmusic"
	case "social network":
		return "social"
	case "streaming":
		return "streaming"
	default:
		return relType
	}
}

func userAgent() string {
	return fmt.Sprintf("Stillwater/%s (https://github.com/sydlexius/stillwater)", version.Version)
}
