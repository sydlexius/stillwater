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
//   - updater.channel:    "stable" | "prerelease" | "nightly"  (default: "stable")
//   - updater.auto_check: "true" | "false"                     (default: "false")
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

	"github.com/sydlexius/stillwater/internal/filesystem"
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
	// ChannelNightly tracks date-stamped nightly releases (nightly-YYYYMMDD).
	// Nightly tags are not semver and are compared lexicographically; the
	// YYYYMMDD suffix orders correctly under plain string comparison.
	ChannelNightly Channel = "nightly"
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
	// RestartRequired is true after a successful Apply has replaced the running
	// binary on disk. It is sticky in-memory: it is set by runApply on success
	// and is cleared only when the process actually restarts (which discards
	// the Service entirely). The UI uses this to show a persistent
	// "restart to finish update" banner after Apply, rather than silently
	// returning to the pre-Apply Apply/Check row.
	RestartRequired bool `json:"restart_required"`
	// PendingVersion is the tag of the binary that was just installed and is
	// waiting for a restart to take effect. Empty unless RestartRequired is true.
	PendingVersion string `json:"pending_version,omitempty"`
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

// nightlyRE matches a nightly release tag of the form "nightly-YYYYMMDD".
// The YYYYMMDD suffix is fixed-width, so lexicographic ordering on the whole
// tag agrees with chronological ordering; pickLatest exploits this to pick
// the newest nightly without a semver parse.
var nightlyRE = regexp.MustCompile(`^nightly-\d{8}$`)

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

	// restartRequired and pendingVersion are set by runApply after a
	// successful binary swap. They are sticky in-memory: nothing else
	// flips them back, so a user who navigates away and back to the
	// Updates tab still sees the "restart required" banner. A real
	// process restart discards the Service and clears them implicitly.
	restartRequired bool
	pendingVersion  string

	// configGen is bumped whenever SetConfig invalidates the cache on a
	// channel change. Check() captures the live value at entry and any
	// attempt to write back fresh release data must still match it; a
	// Check() that started before the switch and finishes after will
	// see a mismatched gen and have its write discarded, preventing
	// old-channel release data from resurrecting in the cache. All
	// access guarded by s.mu alongside the other cache fields.
	configGen uint64

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

// SetDockerForTest overrides the auto-detected Docker flag. Intended for
// testing so suites that cover the non-Docker apply path stay deterministic
// on containerized CI runners (where /.dockerenv would otherwise force the
// service into Docker mode).
func (s *Service) SetDockerForTest(isDocker bool) {
	s.isDocker = isDocker
}

// MarkRestartRequiredForTest exposes the internal restart-required transition
// to tests in other packages (e.g. internal/api) that need to assert the
// post-Apply UI surface without exercising the full runApply pipeline (which
// would attempt to overwrite the test binary). Production code does NOT use
// this; runApply alone calls markRestartRequired after a verified swap.
func (s *Service) MarkRestartRequiredForTest(version string) {
	s.markRestartRequired(version)
}

// detectDocker returns true when the process appears to be running inside a
// Docker (or compatible) container. Minimal base images (distroless,
// chainguard) may omit /.dockerenv and the conventional env vars, so we fall
// back to inspecting /proc/1/cgroup for known runtime signatures. Correctness
// here is a safety floor: a false negative allows Apply to attempt an
// in-place binary swap inside a container, which the Docker-path guard in
// handlePostUpdateApply is meant to prevent.
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
	// Fallback: inspect /proc/1/cgroup on Linux.
	//
	// - macOS / Windows: file does not exist. os.IsNotExist -> fall through
	//   to return false (not a Linux container).
	// - Linux read succeeds: scan each cgroup path for known runtime markers.
	//   Match against path segments (not raw substring) so a systemd unit
	//   named "docker-cleanup.service" in a non-container cgroup does not
	//   false-positive and block binary-apply on that host.
	// - Linux read fails for any other reason (EIO, EACCES, restricted /proc
	//   under AppArmor/SELinux): we cannot confirm *or* deny containerized
	//   execution. The guard in handlePostUpdateApply uses this signal as a
	//   safety floor, so the failure-mode bias is toward "block the in-place
	//   swap." Fail-safe to true so Apply does not proceed on an unknown host.
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		// Fail-safe to true on any non-ENOENT error: see block comment above.
		return !os.IsNotExist(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v1 lines are "hierarchy-id:controller-list:cgroup-path";
		// v2 lines are "0::cgroup-path". Either way the cgroup path is the
		// last colon-separated field, and only that path can legitimately
		// carry a runtime marker.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		for _, marker := range []string{
			"/docker/",        // cgroup v1 Docker daemon
			"docker-",         // cgroup v2 systemd scope (e.g. docker-<id>.scope)
			"/kubepods",       // Kubernetes (kubepods.slice and nested variants)
			"/containerd",     // containerd namespaces
			"/cri-containerd", // containerd via CRI (EKS, GKE)
			"/libpod_parent",  // Podman cgroup v1
			"libpod-",         // Podman systemd slice
			"/lxc/",           // LXC
		} {
			if strings.Contains(path, marker) {
				return true
			}
		}
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
			case ChannelStable, ChannelPrerelease, ChannelNightly:
				cfg.Channel = Channel(v)
			default:
				// A value outside the allowlist means something bypassed
				// SetConfig's validation (direct DB write, failed migration,
				// downgrade from a newer schema). cfg.Channel keeps its
				// ChannelStable default; log loudly so operators can see
				// drift rather than silently rendering "stable" when the
				// user configured something else.
				s.logger.Error("unknown updater.channel value in settings; coercing to stable",
					"stored_value", v)
			}
		case SettingAutoCheck:
			cfg.AutoCheck = v == "true"
		}
	}
	return cfg, rows.Err()
}

// decideChannelChanged reports whether the cached release fields should
// be invalidated given the previous config read. A read error is
// treated as a change (fail-safe): we would rather clear a possibly
// still-valid cache than serve stale channel-specific release metadata
// after a real channel switch whose previous state we could not confirm.
func decideChannelChanged(prev Config, prevErr error, cfg Config) bool {
	if prevErr != nil {
		return true
	}
	return prev.Channel != cfg.Channel
}

// SetConfig persists the updater configuration to the settings table.
// When the channel actually changes, the in-memory cached release fields
// (updateAvailable, latestVersion, releaseURL) are cleared: the new
// channel may have a different latest release, so retaining the old
// channel's cache would advertise a stale "update available" state until
// the next Check, and the sidebar pill (which reads this cache via
// Status) would link to a release from the wrong channel.
func (s *Service) SetConfig(ctx context.Context, cfg Config) error {
	if cfg.Channel != ChannelStable && cfg.Channel != ChannelPrerelease && cfg.Channel != ChannelNightly {
		return fmt.Errorf("invalid channel: %q", cfg.Channel)
	}
	autoCheck := "false"
	if cfg.AutoCheck {
		autoCheck = "true"
	}

	// Read the previous channel so we only invalidate the cache when the
	// channel truly changed. Saving config with the same channel (e.g.
	// toggling auto_check alone) must not flash the sidebar pill away.
	prev, prevErr := s.GetConfig(ctx)
	channelChanged := decideChannelChanged(prev, prevErr, cfg)

	// Wrap both writes in a transaction so a mid-loop failure cannot leave
	// the channel and auto-check settings in a split state.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning updater config transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, kv := range []struct{ k, v string }{
		{SettingChannel, string(cfg.Channel)},
		{SettingAutoCheck, autoCheck},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			kv.k, kv.v, now); err != nil {
			return fmt.Errorf("persisting %q: %w", kv.k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing updater config: %w", err)
	}

	if channelChanged {
		// Bump the generation token and clear the cache in the same
		// critical section: any Check() that captured the pre-bump
		// value will fail the gen check when it tries to write back,
		// so its old-channel release data cannot resurrect here.
		s.mu.Lock()
		s.updateAvailable = false
		s.latestVersion = ""
		s.releaseURL = ""
		s.configGen++
		s.mu.Unlock()
	}
	return nil
}

// storeCheckResult commits a Check() result to the in-memory cache,
// but only if the captured generation still matches the live one.
// Returns true when the write was applied, false when it was skipped
// because SetConfig bumped configGen while this Check() was in flight.
// The guard prevents a Check() launched against the previous channel
// from writing its old-channel release data back into the cache after
// SetConfig cleared it; without this, a channel switch overlapping a
// slow GitHub fetch could re-surface cross-channel release badges.
func (s *Service) storeCheckResult(gen uint64, lastChecked time.Time, available bool, latest, releaseURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configGen != gen {
		return false
	}
	s.lastChecked = lastChecked
	s.updateAvailable = available
	s.latestVersion = latest
	s.releaseURL = releaseURL
	return true
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
		RestartRequired: s.restartRequired,
		PendingVersion:  s.pendingVersion,
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
	// Capture the config generation BEFORE reading cfg. If we captured it
	// after GetConfig, a concurrent SetConfig that commits its tx (new
	// channel visible to GetConfig) and then bumps configGen between the
	// two reads would leave us with a stale cfg but a post-bump gen,
	// and our cache write would incorrectly pass the guard. Capturing
	// first makes the race-window symmetric: any SetConfig that runs
	// after we capture will bump configGen past our captured value,
	// which storeCheckResult will then reject. A legitimate new-channel
	// Check that races with a switch may have its cache write discarded
	// this way, but the caller still receives its CheckResult and the
	// next auto/manual check repopulates the cache correctly.
	s.mu.RLock()
	genAtStart := s.configGen
	s.mu.RUnlock()

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
		// No matching release on this channel: clear cached state and record
		// the successful check so /status reflects the fresh "no update"
		// reality rather than a stale prior version. Latest is returned as
		// the empty string so the CheckResult shape agrees with /status
		// (which reads back these same cleared fields).
		//
		// When the fetched release list is non-empty but pickLatest still
		// found nothing, log a sample of the fetched tag names so operators
		// debugging "my just-published release is not showing up" can see
		// whether the channel is genuinely empty or whether every candidate
		// was filtered (draft, wrong prerelease flag, regex mismatch on the
		// nightly path).
		if len(releases) > 0 {
			const sampleLimit = 5
			sample := make([]string, 0, sampleLimit)
			for i := 0; i < len(releases) && i < sampleLimit; i++ {
				sample = append(sample, releases[i].TagName)
			}
			s.logger.Warn("updater: no release matched channel filter",
				"channel", cfg.Channel, "fetched", len(releases), "sample_tags", sample)
		}
		s.storeCheckResult(genAtStart, time.Now().UTC(), false, "", "")
		return CheckResult{
			Current:         version.Version,
			Latest:          "",
			Channel:         cfg.Channel,
			UpdateAvailable: false,
		}, nil
	}

	available := s.newerThan(latest.TagName, version.Version)
	s.storeCheckResult(genAtStart, time.Now().UTC(), available, latest.TagName, latest.HTMLURL)

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
// manager (systemd, supervisord) restarts it with the new binary. In the
// meantime, Status() reports RestartRequired=true with PendingVersion set
// to the installed tag, and the Settings UI surfaces a persistent
// "restart to finish" banner so users know the Apply succeeded.
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
	if latest == nil || !s.newerThan(latest.TagName, version.Version) {
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
	selfPath, err := executablePath()
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

	// Surface the post-Apply success state. The binary on disk has been
	// replaced; the running process still serves the old version until it is
	// restarted (no in-process re-exec is wired up yet, see #1169 follow-up
	// for an auto-restart hook). Mark the service as restart-required and
	// record the tag of the newly installed binary so the UI can show a
	// persistent "Update installed -- restart Stillwater to finish" banner
	// and the disabled Apply button instead of bouncing back to the
	// pre-Apply Apply/Check row (which previously made a successful Apply
	// look identical to "clicking Apply did nothing").
	s.markRestartRequired(latest.TagName)
	s.setState(StateIdle, 100, "")
}

// executablePath resolves the path of the running binary. Indirected
// through a package var so tests can stub it: the real os.Executable
// returns the test binary path, which the runApply success path would
// then attempt to overwrite (corrupting the test runner). Tests swap
// this for a temp file under t.TempDir(), letting the full apply flow
// (download, checksum, extract, atomic replace, markRestartRequired)
// execute end-to-end without touching the test binary itself.
var executablePath = os.Executable

// markRestartRequired sets the sticky restart-required flag and the pending
// version tag. Held under s.mu so a concurrent Status() reader sees both
// fields update atomically; otherwise the UI could observe restartRequired=true
// with pendingVersion still empty.
func (s *Service) markRestartRequired(version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartRequired = true
	s.pendingVersion = version
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
// It does NOT touch lastChecked: that timestamp is written only by
// storeCheckResult, which runs after a Check() successfully produces
// a result that survives the configGen guard. Writing lastChecked at
// check-start would leave it advanced even when a concurrent channel
// switch caused the check's write to be discarded, making /status
// report "checked just now, no update" indistinguishable from a real
// successful empty check against the new channel.
func (s *Service) setState(st State, progress int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.progress = progress
	s.lastErr = errMsg
}

// pickLatest returns the release that is newest on the requested channel.
// GitHub returns releases in reverse-chronological order, but backported
// releases (e.g. v1.9.9 published after v2.0.0) would be wrongly treated as
// "latest" by a first-match strategy. Scanning all entries and keeping the
// max version ensures correctness regardless of publish order.
//
// Nightly takes a separate path because nightly tags ("nightly-YYYYMMDD")
// are not semver and would be rejected by semverRE/parseSemver. That path
// filters to nightlyRE matches and picks the lexicographic max; the
// fixed-width date suffix makes lex order agree with chronological order.
func pickLatest(releases []githubRelease, ch Channel) *githubRelease {
	if ch == ChannelNightly {
		var best *githubRelease
		for i := range releases {
			r := &releases[i]
			if r.Draft {
				continue
			}
			if !nightlyRE.MatchString(r.TagName) {
				continue
			}
			if best == nil || r.TagName > best.TagName {
				best = r
			}
		}
		return best
	}

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
//
// Semver vs nightly comparison is asymmetric because nightly tags are not
// semver and cannot be meaningfully compared against v1.2.3 on the same
// numeric axis. "Newer" here means "worth advertising as an Update Available
// pill" rather than "strictly greater in some global ordering":
//
//   - both nightly: lex compare on the "nightly-YYYYMMDD" tag (fixed-width
//     date suffix makes lex order agree with chronological order).
//   - only candidate is nightly: user moved from stable/prerelease to
//     nightly; advertise the nightly as the target so the channel switch
//     shows a clickable Apply row.
//   - only current is nightly: user is running a nightly build but has
//     selected a non-nightly channel. The non-nightly channel's latest
//     release is almost always older than the nightly, so do NOT advertise
//     it as an update. Apply still works manually from the Check-result row
//     for users who want to intentionally leave the nightly train; we just
//     do not auto-suggest a downgrade.
//   - neither nightly: fall through to the existing semver comparison.
//
// This is a Service method (not a pure function) so the semver parse-failure
// branch can log. A malformed version.Version ldflag or a hand-rolled tag
// that slipped past pickLatest would otherwise silently return "not newer"
// and an operator debugging "updater says up-to-date but a new release
// exists" would have no breadcrumb.
func (s *Service) newerThan(candidate, current string) bool {
	// Match pickLatest's regex-based detection so "nightly-foobar" or any
	// other HasPrefix-but-not-well-formed input does not sneak into the
	// nightly-aware branches and get lex-compared as if it were a date.
	candNightly := nightlyRE.MatchString(candidate)
	curNightly := nightlyRE.MatchString(current)
	switch {
	case candNightly && curNightly:
		return candidate > current
	case candNightly:
		return true
	case curNightly:
		return false
	}

	cv, err1 := parseSemver(candidate)
	cur, err2 := parseSemver(current)
	if err1 != nil || err2 != nil {
		s.logger.Warn("updater: semver parse failed; treating candidate as not newer",
			"candidate", candidate, "current", current,
			"candidate_err", err1, "current_err", err2)
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

// atomicReplaceFile replaces target with newContent using the project-wide
// tmp/bak/rename helper so binary replacement shares the same durability and
// backup semantics as every other on-disk write (NFO files, settings exports,
// image cache). Preserves the original file's mode so the executable bit is
// retained after the swap.
//
// Threat model: the binary's parent directory must not be attacker-writable.
// filesystem.WriteFileAtomic uses a deterministic "<target>.tmp" path, which
// is adequate because a caller that can plant a symlink at <target>.tmp can
// already overwrite the target binary directly.
func atomicReplaceFile(target string, newContent []byte) error {
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat target: %w", err)
	}
	return filesystem.WriteFileAtomic(target, newContent, fi.Mode())
}
