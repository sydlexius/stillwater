package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Channel represents a release channel filter.
type Channel string

const (
	// ChannelLatest includes only stable (non-prerelease) releases.
	ChannelLatest Channel = "latest"
	// ChannelBeta includes stable releases as well as beta/rc pre-releases.
	ChannelBeta Channel = "beta"
	// ChannelDev includes all releases including dev/nightly and any pre-release.
	ChannelDev Channel = "dev"
)

// AssetInfo describes a downloadable release asset.
type AssetInfo struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
	ChecksumURL string `json:"checksum_url"` // URL to the .sha256 file, may be empty
	Size        int64  `json:"size"`
}

// UpdateInfo is the result of an update check.
type UpdateInfo struct {
	Available   bool        `json:"available"`
	Current     string      `json:"current"`
	Latest      string      `json:"latest,omitempty"`
	Channel     Channel     `json:"channel"`
	ReleaseURL  string      `json:"release_url,omitempty"`
	ReleaseDate time.Time   `json:"release_date,omitempty"`
	Changelog   string      `json:"changelog,omitempty"`
	Assets      []AssetInfo `json:"assets,omitempty"`
}

// githubRelease is the GitHub Releases API response shape.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Prerelease  bool          `json:"prerelease"`
	Draft       bool          `json:"draft"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt time.Time     `json:"published_at"`
	Body        string        `json:"body"`
	Assets      []githubAsset `json:"assets"`
}

// githubAsset is one downloadable file in a GitHub release.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Checker polls the GitHub Releases API and caches the latest result.
type Checker struct {
	repo    string // "owner/repo"
	channel Channel
	current Version
	client  *http.Client
	logger  *slog.Logger

	mu        sync.RWMutex
	cached    *UpdateInfo
	checkedAt time.Time
}

// NewChecker creates a Checker for the given repository and current version.
// client may be nil; a default client with a 15 s timeout will be used.
func NewChecker(repo string, channel Channel, current Version, client *http.Client, logger *slog.Logger) *Checker {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Checker{
		repo:    repo,
		channel: channel,
		current: current,
		client:  client,
		logger:  logger,
	}
}

// Check queries the GitHub Releases API and returns update availability.
// Results are not cached by this method; callers that want caching should use
// CachedCheck or the background scheduler.
func (c *Checker) Check(ctx context.Context) (*UpdateInfo, error) {
	releases, err := c.fetchReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}

	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		if !c.matchesChannel(rel) {
			continue
		}
		tag := strings.TrimPrefix(rel.TagName, "v")
		v, err := Parse(tag)
		if err != nil {
			c.logger.Debug("skipping release with unparseable tag",
				"tag", rel.TagName, "error", err)
			continue
		}
		if v.GT(c.current) {
			info := &UpdateInfo{
				Available:   true,
				Current:     c.current.String(),
				Latest:      v.String(),
				Channel:     c.channel,
				ReleaseURL:  rel.HTMLURL,
				ReleaseDate: rel.PublishedAt,
				Changelog:   rel.Body,
				Assets:      mapAssets(rel.Assets),
			}
			c.store(info)
			return info, nil
		}
		// First matching release is not newer; nothing to update.
		break
	}

	info := &UpdateInfo{Available: false, Current: c.current.String(), Channel: c.channel}
	c.store(info)
	return info, nil
}

// CachedResult returns the most recently cached check result and the time it
// was fetched. Both values are zero if no check has been performed yet.
func (c *Checker) CachedResult() (*UpdateInfo, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached, c.checkedAt
}

// SetChannel updates the release channel. The cached result is discarded so
// that the next CachedCheck or Check uses the new channel.
func (c *Checker) SetChannel(ch Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channel != ch {
		c.channel = ch
		c.cached = nil
		c.checkedAt = time.Time{}
	}
}

// Channel returns the current release channel.
func (c *Checker) Channel() Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channel
}

// StartScheduler runs periodic update checks in the background until ctx is
// cancelled. interval must be positive; values smaller than 1 h are clamped to
// 1 h to avoid hammering the GitHub API.
func (c *Checker) StartScheduler(ctx context.Context, interval time.Duration) {
	const minInterval = time.Hour
	if interval < minInterval {
		interval = minInterval
	}
	c.logger.Info("update checker started", "interval", interval, "channel", c.channel)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("update checker stopped")
			return
		case <-ticker.C:
			if _, err := c.Check(ctx); err != nil {
				c.logger.Warn("periodic update check failed", "error", err)
			}
		}
	}
}

// store writes info to the cache under the write lock.
func (c *Checker) store(info *UpdateInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = info
	c.checkedAt = time.Now().UTC()
}

// fetchReleases calls the GitHub Releases API and returns up to the first 20
// releases (sorted newest-first by the API).
func (c *Checker) fetchReleases(ctx context.Context) ([]githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=20", c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return releases, nil
}

// matchesChannel reports whether a GitHub release should be considered for the
// configured channel.
//
// Channel rules:
//   - latest: non-prerelease only
//   - beta:   non-prerelease OR tag contains "-beta" or "-rc"
//   - dev:    all releases
func (c *Checker) matchesChannel(rel githubRelease) bool {
	tag := strings.ToLower(rel.TagName)
	switch c.channel {
	case ChannelLatest:
		return !rel.Prerelease
	case ChannelBeta:
		if !rel.Prerelease {
			return true
		}
		return strings.Contains(tag, "-beta") || strings.Contains(tag, "-rc")
	case ChannelDev:
		return true
	default:
		return !rel.Prerelease
	}
}

// mapAssets converts GitHub asset objects to AssetInfo, pairing binary assets
// with their corresponding .sha256 checksum files when present.
func mapAssets(assets []githubAsset) []AssetInfo {
	// Build an index of checksum URLs by the base name they cover.
	checksums := make(map[string]string, len(assets))
	for _, a := range assets {
		if strings.HasSuffix(a.Name, ".sha256") {
			base := strings.TrimSuffix(a.Name, ".sha256")
			checksums[base] = a.BrowserDownloadURL
		}
	}

	var result []AssetInfo
	for _, a := range assets {
		if strings.HasSuffix(a.Name, ".sha256") {
			continue // checksum files are not listed as separate assets
		}
		result = append(result, AssetInfo{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			ChecksumURL: checksums[a.Name],
			Size:        a.Size,
		})
	}
	return result
}
