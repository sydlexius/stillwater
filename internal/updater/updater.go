// Package updater provides self-update functionality for Stillwater binaries.
//
// Architecture:
//   - Service holds configuration and state for the update lifecycle.
//   - Check queries the GitHub Releases API for the latest version on the
//     configured channel (stable or prerelease).
//   - Apply downloads the release asset, verifies its SHA256 checksum against
//     the published checksums file, atomically replaces the running binary,
//     and records a restart-required state. The actual process restart is the
//     caller's responsibility (stub hook).
//   - Docker detection skips the in-place apply path; users are shown
//     container-appropriate re-pull instructions instead.
//
// Config keys stored in the settings table:
//   - updater.channel:    "stable" | "prerelease"  (default: "stable")
//   - updater.auto_check: "true" | "false"          (default: "false")
package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/version"
)

// settingKey constants for the settings KV table.
const (
	SettingChannel   = "updater.channel"
	SettingAutoCheck = "updater.auto_check"
)

// ErrAlreadyRunning is returned by Apply when another apply is already in progress.
var ErrAlreadyRunning = errors.New("update already in progress")

// Channel identifies the release channel to track.
type Channel string

const (
	// ChannelStable tracks non-prerelease semver tags (v1.2.3).
	ChannelStable Channel = "stable"
	// ChannelPrerelease tracks prerelease tags (v1.2.3-rc.1, v1.2.3-beta.1).
	ChannelPrerelease Channel = "prerelease"
)

// State describes the current phase of the update lifecycle.
type State string

const (
	// StateIdle means no update operation is in progress.
	StateIdle State = "idle"
	// StateChecking means the service is querying GitHub for release info.
	StateChecking State = "checking"
	// StateDownloading means the release asset is being downloaded.
	StateDownloading State = "downloading"
	// StateApplying means the downloaded binary is being installed.
	StateApplying State = "applying"
	// StateError means the most recent operation failed; see StatusResult.Error.
	StateError State = "error"
)

// Config holds user-configurable update preferences.
type Config struct {
	Channel   Channel `json:"channel"`
	AutoCheck bool    `json:"auto_check"`
}

// CheckResult is returned by Check.
type CheckResult struct {
	Current         string  `json:"current"`
	Latest          string  `json:"latest"`
	Channel         Channel `json:"channel"`
	UpdateAvailable bool    `json:"update_available"`
	ReleaseURL      string  `json:"release_url,omitempty"`
	PublishedAt     string  `json:"published_at,omitempty"`
}

// StatusResult is returned by Status.
type StatusResult struct {
	State           State  `json:"state"`
	Progress        int    `json:"progress"` // 0-100 percent
	Error           string `json:"error,omitempty"`
	LastChecked     string `json:"last_checked,omitempty"` // RFC 3339
	IsDocker        bool   `json:"is_docker"`
	UpdateAvailable bool   `json:"update_available"`
	Latest          string `json:"latest,omitempty"`      // latest version tag from last check
	ReleaseURL      string `json:"release_url,omitempty"` // GitHub release page URL
}

// githubRelease is a subset of the GitHub Releases API response.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Prerelease  bool          `json:"prerelease"`
	Draft       bool          `json:"draft"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt string        `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

// githubAsset represents a single release asset.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// semverRE matches a simple semver tag with optional pre-release suffix.
var semverRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[a-zA-Z0-9.]+)?$`)

// Service manages the update lifecycle.
type Service struct {
	db         *sql.DB
	httpClient *http.Client
	logger     *slog.Logger

	mu              sync.RWMutex
	state           State
	progress        int
	lastErr         string
	lastChecked     time.Time
	isDocker        bool
	updateAvailable bool
	latestVersion   string
	releaseURL      string // URL to the GitHub release page for the latest version

	// applyRunning guards against concurrent Apply calls. 0 = idle, 1 = running.
	// Using atomic.Int32 makes the idle-check and the transition to running a
	// single indivisible operation (CompareAndSwap), eliminating the TOCTOU race
	// where two callers could both pass the idle check before either launches.
	applyRunning atomic.Int32
}

// NewService creates a new updater Service.
func NewService(db *sql.DB, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		state:    StateIdle,
		isDocker: detectDocker(),
	}
}

// NewDockerService creates a Service that reports itself as Docker-hosted.
// Intended for testing and environments where the auto-detection cannot be used.
func NewDockerService(db *sql.DB, logger *slog.Logger) *Service {
	svc := NewService(db, logger)
	svc.isDocker = true
	return svc
}

// SetHTTPClient replaces the internal HTTP client. Intended for testing.
func (s *Service) SetHTTPClient(c *http.Client) {
	s.httpClient = c
}

// detectDocker returns true when the process appears to be running inside a
// Docker container. It checks for the /.dockerenv file and the container env var.
func detectDocker() bool {
	// Presence of /.dockerenv is the canonical Docker indicator.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// container=docker is set by some container runtimes.
	if v := os.Getenv("container"); v == "docker" {
		return true
	}
	// DOCKER_CONTAINER is a common convention in custom Docker images.
	if v := os.Getenv("DOCKER_CONTAINER"); v != "" {
		return true
	}
	return false
}

// GetConfig reads the updater configuration from the settings table.
// Returns sensible defaults when keys are absent.
func (s *Service) GetConfig(ctx context.Context) (Config, error) {
	cfg := Config{
		Channel:   ChannelStable,
		AutoCheck: false,
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key IN (?, ?)`,
		SettingChannel, SettingAutoCheck)
	if err != nil {
		return cfg, fmt.Errorf("querying updater config: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return cfg, fmt.Errorf("scanning updater config: %w", err)
		}
		switch k {
		case SettingChannel:
			switch Channel(v) {
			case ChannelStable, ChannelPrerelease:
				cfg.Channel = Channel(v)
			}
		case SettingAutoCheck:
			cfg.AutoCheck = v == "true"
		}
	}
	return cfg, rows.Err()
}

// SetConfig persists the updater configuration to the settings table.
func (s *Service) SetConfig(ctx context.Context, cfg Config) error {
	if cfg.Channel != ChannelStable && cfg.Channel != ChannelPrerelease {
		return fmt.Errorf("invalid channel: %q", cfg.Channel)
	}
	autoCheck := "false"
	if cfg.AutoCheck {
		autoCheck = "true"
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, kv := range []struct{ k, v string }{
		{SettingChannel, string(cfg.Channel)},
		{SettingAutoCheck, autoCheck},
	} {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			kv.k, kv.v, now)
		if err != nil {
			return fmt.Errorf("persisting %q: %w", kv.k, err)
		}
	}
	return nil
}

// Status returns a snapshot of the current update state.
func (s *Service) Status() StatusResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := StatusResult{
		State:           s.state,
		Progress:        s.progress,
		Error:           s.lastErr,
		IsDocker:        s.isDocker,
		UpdateAvailable: s.updateAvailable,
		Latest:          s.latestVersion,
		ReleaseURL:      s.releaseURL,
	}
	if !s.lastChecked.IsZero() {
		res.LastChecked = s.lastChecked.UTC().Format(time.RFC3339)
	}
	return res
}

// IsDocker returns whether the service detected a Docker environment.
func (s *Service) IsDocker() bool {
	return s.isDocker
}

// Check queries the GitHub Releases API for the latest version on the
// configured channel. It sets the internal state to StateChecking during
// the request.
func (s *Service) Check(ctx context.Context) (CheckResult, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return CheckResult{}, fmt.Errorf("reading config: %w", err)
	}

	s.setState(StateChecking, 0, "")
	defer func() {
		// Reset to idle after check completes (success or failure).
		// Only reset if still in checking state to avoid clobbering Apply state.
		s.mu.Lock()
		if s.state == StateChecking {
			s.state = StateIdle
		}
		s.mu.Unlock()
	}()

	releases, err := s.fetchReleases(ctx)
	if err != nil {
		s.setState(StateError, 0, err.Error())
		return CheckResult{}, fmt.Errorf("fetching releases: %w", err)
	}

	latest := pickLatest(releases, cfg.Channel)
	if latest == nil {
		// No matching release: clear any stale update flag so /status does not
		// keep advertising an update that is no longer present on this channel.
		s.mu.Lock()
		s.updateAvailable = false
		s.latestVersion = ""
		s.releaseURL = ""
		s.mu.Unlock()
		return CheckResult{
			Current:         version.Version,
			Latest:          version.Version,
			Channel:         cfg.Channel,
			UpdateAvailable: false,
		}, nil
	}

	available := newerThan(latest.TagName, version.Version)

	s.mu.Lock()
	s.lastChecked = time.Now().UTC()
	s.updateAvailable = available
	s.latestVersion = latest.TagName
	s.releaseURL = latest.HTMLURL
	s.mu.Unlock()

	return CheckResult{
		Current:         version.Version,
		Latest:          latest.TagName,
		Channel:         cfg.Channel,
		UpdateAvailable: available,
		ReleaseURL:      latest.HTMLURL,
		PublishedAt:     latest.PublishedAt,
	}, nil
}

// Apply downloads and atomically installs the latest release binary.
// Returns an error if the environment is Docker (callers should check
// IsDocker before calling). The apply runs asynchronously; use Status()
// to poll progress.
//
// Restart hook: after a successful apply, the binary has been replaced on
// disk but the running process is NOT automatically restarted. A future
// enhancement should send SIGTERM to self (or call os.Exit) so the process
// manager (systemd, supervisord) restarts it with the new binary.
func (s *Service) Apply(ctx context.Context) error {
	if s.isDocker {
		return fmt.Errorf("binary update is not supported in Docker environments; re-pull the container image instead")
	}

	// CompareAndSwap atomically checks that no apply is running (0) and sets it
	// to running (1) in one step. This eliminates the TOCTOU window that existed
	// when the check and the goroutine launch were two separate operations.
	if !s.applyRunning.CompareAndSwap(0, 1) {
		return ErrAlreadyRunning
	}

	// Use a background context so the apply goroutine outlives the initiating
	// HTTP request. The handler already detaches via context.WithoutCancel, but
	// using context.Background() here makes the intent explicit at the service
	// layer and avoids any inherited deadline or cancellation from the caller.
	go s.runApply(context.Background()) //nolint:gosec // G118: intentional -- goroutine must outlive request context
	return nil
}

// runApply is the internal goroutine body for Apply.
func (s *Service) runApply(ctx context.Context) {
	// Always clear the running flag when we exit, so Apply can be called again.
	defer s.applyRunning.Store(0)

	cfg, err := s.GetConfig(ctx)
	if err != nil {
		s.setState(StateError, 0, "reading config: "+err.Error())
		return
	}

	s.setState(StateChecking, 0, "")

	releases, err := s.fetchReleases(ctx)
	if err != nil {
		s.setState(StateError, 0, "fetching releases: "+err.Error())
		return
	}

	latest := pickLatest(releases, cfg.Channel)
	if latest == nil || !newerThan(latest.TagName, version.Version) {
		s.setState(StateIdle, 100, "")
		return
	}

	s.setState(StateDownloading, 10, "")

	// Find the asset for this platform.
	assetName := binaryAssetName(latest.TagName)
	checksumName := checksumAssetName(latest.TagName)

	var binaryURL, checksumURL string
	for _, a := range latest.Assets {
		switch a.Name {
		case assetName:
			binaryURL = a.BrowserDownloadURL
		case checksumName:
			checksumURL = a.BrowserDownloadURL
		}
	}

	if binaryURL == "" {
		s.setState(StateError, 0, fmt.Sprintf("no release asset found for %s/%s (want %q)", runtime.GOOS, runtime.GOARCH, assetName))
		return
	}

	// Download the checksum file first (small, fast). Fail closed if the release
	// does not ship a checksum manifest or the manifest omits our asset: skipping
	// verification would let a compromised release silently install an unverified
	// binary, which defeats the purpose of the checksum guarantee.
	if checksumURL == "" {
		s.setState(StateError, 0, fmt.Sprintf("checksum asset %q not found in release", checksumName))
		return
	}
	checksumData, err := s.downloadBytes(ctx, checksumURL)
	if err != nil {
		s.setState(StateError, 0, "downloading checksums: "+err.Error())
		return
	}
	expectedChecksum := parseChecksum(checksumData, assetName)
	if expectedChecksum == "" {
		s.setState(StateError, 0, fmt.Sprintf("checksum for asset %q not found in %q", assetName, checksumName))
		return
	}

	s.setState(StateDownloading, 30, "")

	// Download the binary archive.
	binaryData, err := s.downloadBytes(ctx, binaryURL)
	if err != nil {
		s.setState(StateError, 0, "downloading binary: "+err.Error())
		return
	}

	s.setState(StateDownloading, 70, "")

	// Verify checksum.
	actual := sha256Hex(binaryData)
	if !strings.EqualFold(actual, expectedChecksum) {
		s.setState(StateError, 0, fmt.Sprintf("checksum mismatch: expected %s, got %s", expectedChecksum, actual))
		return
	}
	s.logger.Info("checksum verified", "asset", assetName, "sha256", actual)

	s.setState(StateApplying, 80, "")

	// Extract the binary from the archive (tarball).
	newBinary, err := extractBinary(binaryData)
	if err != nil {
		s.setState(StateError, 0, "extracting binary: "+err.Error())
		return
	}

	// Atomic replacement of the running binary.
	selfPath, err := os.Executable()
	if err != nil {
		s.setState(StateError, 0, "resolving executable path: "+err.Error())
		return
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		s.setState(StateError, 0, "resolving symlinks for executable: "+err.Error())
		return
	}

	if err := atomicReplaceFile(selfPath, newBinary); err != nil {
		s.setState(StateError, 0, "replacing binary: "+err.Error())
		return
	}

	s.logger.Info("binary updated successfully", "version", latest.TagName, "path", selfPath)

	// NOTE: Restart hook (stub). A future enhancement should signal the process
	// to restart so the new binary takes effect. For now, a manual restart is
	// required. This is acceptable for RC-level delivery; the status endpoint
	// returns state=idle with no error, and the UI can advise the user to restart.
	s.setState(StateIdle, 100, "")
}

// fetchReleases calls the GitHub Releases API for this repository. The page
// size is 100 (GitHub's maximum) so the stable channel still sees the most
// recent stable release even when many prereleases have been published since.
// GitHub returns releases in reverse-chronological order; pickLatest scans
// this slice for the newest entry matching the requested channel.
func (s *Service) fetchReleases(ctx context.Context) ([]githubRelease, error) {
	const apiURL = "https://api.github.com/repos/sydlexius/stillwater/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "stillwater/"+version.Version)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github API request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("github API rate limited (status %d); try again later", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned status %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding github response: %w", err)
	}
	return releases, nil
}

// downloadBytes fetches the resource at rawURL and returns its contents.
// Only https:// URLs are accepted; http:// or other schemes are rejected to
// prevent a compromised API response from redirecting downloads to an
// unencrypted or local path.
func (s *Service) downloadBytes(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing download URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("download URL must use https, got %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "stillwater/"+version.Version)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20)) // 200 MB cap
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return data, nil
}

// setState updates the internal state fields under the lock.
func (s *Service) setState(st State, progress int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.progress = progress
	s.lastErr = errMsg
	// Mark last-checked at the start of a check cycle.
	if st == StateChecking {
		s.lastChecked = time.Now().UTC()
	}
}

// pickLatest returns the release with the highest semantic version that matches
// the channel. GitHub returns releases in reverse-chronological order, but
// backported releases (e.g. v1.9.9 published after v2.0.0) would be wrongly
// treated as "latest" by a first-match strategy. Scanning all entries and
// keeping the max semver ensures correctness regardless of publish order.
func pickLatest(releases []githubRelease, ch Channel) *githubRelease {
	var best *githubRelease
	var bestVer semver

	for i := range releases {
		r := &releases[i]
		if r.Draft {
			continue
		}
		if !semverRE.MatchString(r.TagName) {
			continue
		}

		switch ch {
		case ChannelStable:
			// Stable: no prerelease suffix in tag AND GitHub prerelease=false.
			if r.Prerelease {
				continue
			}
			tag := strings.TrimPrefix(r.TagName, "v")
			if strings.ContainsAny(tag, "-") {
				continue // Has a prerelease suffix like -rc.1
			}
		case ChannelPrerelease:
			// Prerelease channel includes stable AND prerelease tags; no filtering.
		default:
			continue
		}

		v, err := parseSemver(r.TagName)
		if err != nil {
			continue
		}
		if best == nil || semverCompare(v, bestVer) > 0 {
			best = r
			bestVer = v
		}
	}
	return best
}

// newerThan returns true when candidate is strictly newer than current.
// Comparison is done via parseSemver.
func newerThan(candidate, current string) bool {
	cv, err1 := parseSemver(candidate)
	cur, err2 := parseSemver(current)
	if err1 != nil || err2 != nil {
		return false
	}
	return semverCompare(cv, cur) > 0
}

// binaryAssetName returns the expected asset filename for the current
// platform. Matches the GoReleaser default naming convention:
// stillwater_<version>_<os>_<arch>.tar.gz
func binaryAssetName(tagName string) string {
	ver := strings.TrimPrefix(tagName, "v")
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	return fmt.Sprintf("stillwater_%s_%s_%s.tar.gz", ver, goos, goarch)
}

// checksumAssetName returns the checksum file name for a release.
func checksumAssetName(tagName string) string {
	ver := strings.TrimPrefix(tagName, "v")
	return fmt.Sprintf("stillwater_%s_checksums.txt", ver)
}

// parseChecksum extracts the SHA256 hex digest for the given filename
// from a standard GoReleaser checksums.txt content.
// Format per line: "<sha256hex>  <filename>"
func parseChecksum(data []byte, filename string) string {
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == filename {
			return parts[0]
		}
	}
	return ""
}

// sha256Hex returns the hex-encoded SHA256 digest of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// extractBinary reads the stillwater binary from a .tar.gz archive.
// It expects exactly one entry named "stillwater" or "stillwater.exe".
func extractBinary(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(newBytesReader(data))
	if err != nil {
		return nil, fmt.Errorf("opening gzip: %w", err)
	}
	defer gr.Close() //nolint:errcheck

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}
		name := filepath.Base(hdr.Name)
		if name == "stillwater" || name == "stillwater.exe" {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
	return nil, fmt.Errorf("binary not found in archive")
}

// newBytesReader wraps a byte slice in an io.Reader.
func newBytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// atomicReplaceFile replaces target with newContent using a tmp+rename pattern.
// It preserves the original file permissions.
//
// The temp file is created with os.CreateTemp (O_EXCL, unpredictable name) in
// the same directory as the target. Using a predictable name like "target.new"
// would allow an attacker to pre-create a symlink and redirect the write to an
// arbitrary file. CreateTemp sidesteps that by generating an unpredictable name
// and opening it with O_EXCL so the create fails if the name already exists.
func atomicReplaceFile(target string, newContent []byte) error {
	// Preserve permissions of the current binary.
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat target: %w", err)
	}
	perm := fi.Mode()

	// Create the temp file in the same directory as the target so that
	// os.Rename stays on the same filesystem (cross-device rename would fail).
	tmp, err := os.CreateTemp(filepath.Dir(target), "stillwater-*.new")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// If anything goes wrong after this point, clean up the temp file.
	var writeErr error
	defer func() {
		if writeErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		writeErr = err
		_ = tmp.Close()
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if _, err := tmp.Write(newContent); err != nil {
		writeErr = err
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		writeErr = err
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Rename is atomic on most Unix filesystems (POSIX rename(2) guarantee).
	if err := os.Rename(tmpPath, target); err != nil {
		writeErr = err
		return fmt.Errorf("renaming to target: %w", err)
	}
	return nil
}
