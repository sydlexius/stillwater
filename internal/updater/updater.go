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
//   - updater.channel:              "stable" | "prerelease" | "nightly"  (default: "stable")
//   - updater.enabled:              "true" | "false"                     (default: "true")
//   - updater.auto_check:           "true" | "false"                     (default: "false")
//   - updater.check_interval_hours: integer string, minimum 1            (default: "24")
//
// Auto-Apply on the non-Docker path is wired through Config.AutoUpdate. The
// scheduler calls Apply() automatically when AutoUpdate is enabled, the
// release is newer than version.Version, the host is not Docker, and the
// candidate tag is not present in Config.SkippedVersions. Docker hosts are a
// no-op (orchestration handles updates).
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/version"
)

// settingKey constants for the settings KV table.
const (
	SettingChannel            = "updater.channel"
	SettingEnabled            = "updater.enabled"
	SettingAutoCheck          = "updater.auto_check"
	SettingAutoUpdate         = "updater.auto_update"
	SettingCheckIntervalHours = "updater.check_interval_hours"
	SettingLastAutoApplied    = "updater.last_auto_applied"         // RFC3339 timestamp
	SettingLastAutoAppliedVer = "updater.last_auto_applied_version" // tag string
	SettingSkippedVersions    = "updater.skipped_versions"          // JSON-encoded []string
)

// schedulerHourUnit is the multiplier applied to Config.CheckIntervalHours
// when computing the scheduler's tick interval. Production code leaves this
// at time.Hour. Tests swap it to a small unit (e.g. time.Millisecond) so
// scheduler reactivity can be exercised without sleeping for an hour.
//
// Package-private and intentionally a var, not a const: tests in this same
// package set it inside t.Cleanup. External callers cannot reach it.
var schedulerHourUnit = time.Hour

// MinCheckIntervalHours is the floor for the background auto-check cadence.
// A 1h floor prevents users from accidentally hammering the GitHub Releases
// API (60 requests/hour from a single host is well under the 60/hr unauth
// rate limit, but tighter intervals would risk rate-limit responses for
// no real benefit).
const MinCheckIntervalHours = 1

// DefaultCheckIntervalHours is used when no override is stored or a stored
// value falls below the minimum. 24h matches the cadence other Stillwater
// background scanners use by default and is appropriate for a release feed
// that updates at most a few times per week.
const DefaultCheckIntervalHours = 24

// ErrAlreadyRunning is returned by Apply when another apply is already in progress.
var ErrAlreadyRunning = errors.New("update already in progress")

// ErrRestartRequired is returned by Apply when a previous apply has already
// staged a new binary and the process has not yet restarted. The in-memory
// version.Version still reports the pre-apply tag, so without this guard a
// second Apply call would treat the same release as newer and rerun the full
// download/replace path on top of the already-staged binary.
var ErrRestartRequired = errors.New("restart required before applying another update")

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
//
// Two orthogonal knobs gate the background loop:
//
//   - Enabled: top-level kill switch. When false, the background loop is a
//     no-op AND manual Apply is rejected (the API surface gates on Enabled
//     so an admin who flips this off is fully opted out of in-app updates).
//   - AutoCheck: when true and Enabled is also true, the background loop
//     polls GitHub at CheckIntervalHours.
//
// AutoUpdate, when true together with Enabled and AutoCheck, makes the
// scheduler call Apply() automatically after a successful Check finds a
// newer release. Docker hosts are a no-op even when AutoUpdate is true
// (binary in-place swap is unsupported in containers; orchestration is
// expected to handle image refresh). The safety surface for AutoUpdate
// (first-toggle confirmation modal, last-auto-applied status display,
// skip-this-version affordance) lives in the Settings UI.
type Config struct {
	Channel            Channel `json:"channel"`
	Enabled            bool    `json:"enabled"`
	AutoCheck          bool    `json:"auto_check"`
	AutoUpdate         bool    `json:"auto_update"`
	CheckIntervalHours int     `json:"check_interval_hours"`

	// LastAutoApplied is the time the scheduler last successfully called
	// Apply() automatically. Zero value means no auto-apply has occurred.
	// Read-only from a client's perspective: written by markAutoApplied
	// after a successful runApply triggered by the scheduler. Surfaced in
	// the Updates tab as a "last auto-applied: vX.Y.Z, 2h ago" line.
	LastAutoApplied time.Time `json:"last_auto_applied,omitempty"`

	// LastAutoAppliedVersion is the tag of the release that was installed
	// in the last auto-apply. Empty when LastAutoApplied is zero.
	LastAutoAppliedVersion string `json:"last_auto_applied_version,omitempty"`

	// SkippedVersionsJSON tag note: the field intentionally omits
	// ,omitempty so the key is always present in the response. The empty
	// shape is guaranteed by initializing the slice to []string{} (not
	// nil) wherever Config is constructed -- a nil slice marshals to
	// "null", not "[]". This matches the dedicated /updates/skips
	// endpoint and lets the UI's compositional auto-save flow round-trip
	// the field without defensive nil checks.
	//
	// SkippedVersions is the persisted list of release tags that the
	// scheduler must NOT auto-apply. The skip-this-version button
	// appends to this list; the scheduler honors it on every tick by
	// short-circuiting auto-apply when the candidate tag is present.
	// Stored as a JSON array of strings under SettingSkippedVersions.
	SkippedVersions []string `json:"skipped_versions"`
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

	// skippedVersionsMu serializes the read-modify-write sequence in
	// AddSkippedVersion / RemoveSkippedVersion so two concurrent admin
	// requests cannot lose an update by both reading the same snapshot
	// and clobbering each other on writeSkippedVersions. Held across the
	// entire GetConfig -> mutate -> persist window.
	skippedVersionsMu sync.Mutex

	// configChange is a buffered (capacity 1) signal channel that SetConfig
	// pulses non-blockingly after a successful persist. The scheduler
	// loop selects on it alongside the timer, so a config change wakes
	// the scheduler immediately instead of waiting out the previous
	// (possibly 24h) interval. Capacity 1 with a non-blocking send means
	// rapid back-to-back changes coalesce into a single wakeup, which is
	// the correct behavior: the scheduler always re-reads the freshest
	// config from the DB on wake.
	configChange chan struct{}
}

// NewService creates a new updater Service.
func NewService(db *sql.DB, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		state:        StateIdle,
		isDocker:     detectDocker(),
		configChange: make(chan struct{}, 1),
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

// MarkAutoAppliedForTest exposes the internal markAutoApplied write so
// cross-package tests (e.g. internal/api) can seed a "last auto-applied"
// settings row without driving the full Apply pipeline. Production code
// does NOT use this; the scheduler's applyAuto branch is the only caller
// of markAutoApplied in the running service.
func (s *Service) MarkAutoAppliedForTest(ctx context.Context, version string) error {
	return s.markAutoApplied(ctx, version)
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
//
// Defaults:
//   - Channel:            ChannelStable
//   - Enabled:            true (the updater itself is on; AutoCheck is
//     still individually opt-in)
//   - AutoCheck:          false
//   - CheckIntervalHours: DefaultCheckIntervalHours (24h)
func (s *Service) GetConfig(ctx context.Context) (Config, error) {
	cfg := Config{
		Channel:            ChannelStable,
		Enabled:            true,
		AutoCheck:          false,
		AutoUpdate:         false,
		CheckIntervalHours: DefaultCheckIntervalHours,
		// Initialize to empty slice (not nil) so JSON output is "[]"
		// rather than "null" when no skipped_versions row exists.
		SkippedVersions: []string{},
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?, ?, ?, ?, ?)`,
		SettingChannel, SettingEnabled, SettingAutoCheck, SettingAutoUpdate, SettingCheckIntervalHours,
		SettingLastAutoApplied, SettingLastAutoAppliedVer, SettingSkippedVersions)
	if err != nil {
		return cfg, fmt.Errorf("querying updater config: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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
		case SettingEnabled:
			// Stored only when explicitly toggled; absence keeps the
			// "enabled" default so existing installs are not silently
			// disabled when the new key rolls out. ParseBool (rather than
			// `v == "true"`) accepts the broader set of strconv-recognized
			// values written by older or out-of-band migrations ("1",
			// "TRUE", "T", etc.); a malformed value preserves the default
			// rather than silently flipping the kill switch off.
			if b, err := strconv.ParseBool(v); err == nil {
				cfg.Enabled = b
			} else {
				s.logger.Warn("invalid updater.enabled value in settings; keeping default",
					"stored_value", v)
			}
		case SettingAutoCheck:
			if b, err := strconv.ParseBool(v); err == nil {
				cfg.AutoCheck = b
			} else {
				s.logger.Warn("invalid updater.auto_check value in settings; keeping default",
					"stored_value", v)
			}
		case SettingAutoUpdate:
			if b, err := strconv.ParseBool(v); err == nil {
				cfg.AutoUpdate = b
			} else {
				s.logger.Warn("invalid updater.auto_update value in settings; keeping default",
					"stored_value", v)
			}
		case SettingLastAutoApplied:
			// RFC3339; absence (zero time) is the documented default.
			// A malformed value falls back to zero rather than failing the
			// whole config read so the rest of the page can still render.
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				cfg.LastAutoApplied = t
			} else {
				s.logger.Warn("invalid updater.last_auto_applied value in settings; keeping zero",
					"stored_value", v, "error", err)
			}
		case SettingLastAutoAppliedVer:
			cfg.LastAutoAppliedVersion = v
		case SettingSkippedVersions:
			// JSON array of strings. An unparsable value yields an empty
			// list (no skips) rather than a config-read failure, matching
			// the fail-open philosophy of the rest of GetConfig.
			if v != "" {
				var skipped []string
				if err := json.Unmarshal([]byte(v), &skipped); err == nil {
					cfg.SkippedVersions = skipped
				} else {
					s.logger.Warn("invalid updater.skipped_versions value in settings; treating as empty",
						"stored_value", v, "error", err)
				}
			}
		case SettingCheckIntervalHours:
			n, err := strconv.Atoi(v)
			if err != nil || n < MinCheckIntervalHours {
				// Corrupt or out-of-range stored value falls back to the
				// default rather than blocking the whole updater. Log so
				// operators can see drift; a one-line warn is cheap.
				s.logger.Warn("invalid updater.check_interval_hours value; using default",
					"stored_value", v, "default_hours", DefaultCheckIntervalHours)
				cfg.CheckIntervalHours = DefaultCheckIntervalHours
			} else {
				cfg.CheckIntervalHours = n
			}
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
	// Treat a zero CheckIntervalHours as "use the default" rather than as an
	// error: callers (especially tests and older clients that predate this
	// field) may not populate it. A negative value is still rejected so an
	// explicit garbage write fails loudly.
	if cfg.CheckIntervalHours == 0 {
		cfg.CheckIntervalHours = DefaultCheckIntervalHours
	}
	if cfg.CheckIntervalHours < MinCheckIntervalHours {
		return fmt.Errorf("check_interval_hours must be >= %d", MinCheckIntervalHours)
	}
	enabled := "false"
	if cfg.Enabled {
		enabled = "true"
	}
	autoCheck := "false"
	if cfg.AutoCheck {
		autoCheck = "true"
	}
	autoUpdate := "false"
	if cfg.AutoUpdate {
		autoUpdate = "true"
	}

	// Read the previous channel so we only invalidate the cache when the
	// channel truly changed. Saving config with the same channel (e.g.
	// toggling auto_check alone) must not flash the sidebar pill away.
	prev, prevErr := s.GetConfig(ctx)
	channelChanged := decideChannelChanged(prev, prevErr, cfg)

	// Wrap all writes in a transaction so a mid-loop failure cannot leave
	// the updater settings in a split state.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning updater config transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, kv := range []struct{ k, v string }{
		{SettingChannel, string(cfg.Channel)},
		{SettingEnabled, enabled},
		{SettingAutoCheck, autoCheck},
		{SettingAutoUpdate, autoUpdate},
		{SettingCheckIntervalHours, strconv.Itoa(cfg.CheckIntervalHours)},
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

	// Wake the scheduler so cadence / Enabled / AutoCheck / AutoUpdate
	// changes take effect immediately rather than waiting out the
	// previous (possibly 24h) interval. Non-blocking: capacity is 1
	// and the scheduler always re-reads the freshest config on wake,
	// so coalescing rapid back-to-back changes is correct. If the
	// channel is nil (legacy callers building a Service without
	// NewService), skip silently rather than panic.
	if s.configChange != nil {
		select {
		case s.configChange <- struct{}{}:
		default:
		}
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

	// Reject when a prior apply already staged a new binary. version.Version
	// is a build-time constant that does not change post-Apply, so a second
	// call would otherwise re-download the same release on top of the staged
	// one. Read under RLock so a concurrent markRestartRequired observes a
	// consistent view; we release before the CAS to keep the existing
	// concurrency contract for ErrAlreadyRunning unchanged.
	s.mu.RLock()
	restartRequired := s.restartRequired
	s.mu.RUnlock()
	if restartRequired {
		return ErrRestartRequired
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
	go s.runApply(context.Background(), "") //nolint:gosec,contextcheck // gosec G118 + contextcheck both fire on this line; apply goroutine intentionally outlives request context (handler already detaches via WithoutCancel)
	return nil
}

// runApply is the internal goroutine body for Apply.
//
// pinnedVersion, when non-empty, requires the live-fetched latest
// release to match this exact tag. Used by the auto-apply path so a
// release that drifts between maybeAutoApply's gating decision and the
// goroutine's fetch (e.g. channel switch, newer tag published) is
// rejected with a logged skip rather than silently installed. The
// manual Apply path passes "" (no pin) since the user just clicked
// Apply on whatever the UI currently surfaces as the latest.
func (s *Service) runApply(ctx context.Context, pinnedVersion string) {
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

	// Auto-apply: require the live-fetched candidate to match the tag
	// the scheduler vetted (channel + skip-list) at the gating moment.
	// A mismatch means the channel changed or a newer tag was published
	// in the gap between gating and goroutine execution; bail rather
	// than install a release that did not pass the gate.
	if pinnedVersion != "" && latest.TagName != pinnedVersion {
		s.logger.Info("updater: auto-apply candidate drifted, skipping",
			"vetted", pinnedVersion, "live", latest.TagName)
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
	req.Header.Set("User-Agent", version.UserAgent("", ""))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github API request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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
	req.Header.Set("User-Agent", version.UserAgent("", ""))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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
	defer gr.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// StartScheduler runs the background auto-check loop. It blocks until ctx is
// canceled. Callers typically launch it in a goroutine from main.
//
// Behavior on each tick:
//
//   - Reload the live Config from settings so admins can change cadence /
//     toggle Enabled / AutoCheck / AutoUpdate.
//   - When Enabled and AutoCheck are both true, run Check(). The result
//     populates the in-memory cache that drives the sidebar "update
//     available" pill.
//   - When AutoUpdate is also true, the host is not Docker, the latest
//     release is newer than version.Version, and the candidate tag is
//     not in SkippedVersions, call Apply() automatically and persist the
//     last-auto-applied marker on success.
//
// Reactivity: SetConfig pulses s.configChange after persisting; the loop
// selects on it alongside the timer, so a cadence change (24h to 1h) or
// Enabled/AutoCheck/AutoUpdate toggle takes effect on the very next
// scheduler iteration rather than waiting out the previous interval.
func (s *Service) StartScheduler(ctx context.Context) {
	// Initial read picks up the persisted interval. Fall back to the
	// default on read error so a transient DB hiccup at startup does
	// not silently disable the loop.
	cfg, err := s.GetConfig(ctx)
	interval := time.Duration(DefaultCheckIntervalHours) * schedulerHourUnit
	if err == nil && cfg.CheckIntervalHours >= MinCheckIntervalHours {
		interval = time.Duration(cfg.CheckIntervalHours) * schedulerHourUnit
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	s.logger.Info("updater scheduler started",
		"initial_interval", interval.String())

	// resetTimer stops, drains, and resets the timer to a fresh interval
	// computed from the current config. Centralized here so both the
	// timer-fired branch and the configChange-fired branch share the
	// exact same drain pattern (Go's time.Timer requires the receive on
	// .C to be guarded by the bool returned from Stop()).
	resetTimer := func(next time.Duration) {
		if !timer.Stop() {
			// Stop returns false when the timer has already fired or
			// been stopped previously. Drain only when a value is
			// actually pending; a non-blocking select prevents the
			// drain from deadlocking when the channel is empty (which
			// is the normal case after timer.C just fired in the outer
			// select).
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("updater scheduler stopped")
			return

		case <-s.configChange:
			// Config-change wakeup: re-read config and reset the timer
			// to the new interval. Do NOT run a Check on the wakeup
			// itself (the user may have just toggled AutoCheck off);
			// the next regular tick handles the work.
			cfg, err := s.GetConfig(ctx)
			next := time.Duration(DefaultCheckIntervalHours) * schedulerHourUnit
			if err == nil && cfg.CheckIntervalHours >= MinCheckIntervalHours {
				next = time.Duration(cfg.CheckIntervalHours) * schedulerHourUnit
			}
			interval = next
			resetTimer(interval)
			s.logger.Debug("updater scheduler: config changed; timer reset",
				"new_interval", interval.String())

		case <-timer.C:
			cfg, err := s.GetConfig(ctx)
			if err != nil {
				s.logger.Warn("updater scheduler: reading config failed; skipping tick",
					"error", err)
				resetTimer(interval)
				continue
			}

			if cfg.Enabled && cfg.AutoCheck {
				result, err := s.Check(ctx)
				if err != nil {
					// Check already records the error in StatusResult.
					// A debug-level log here is enough to leave a
					// breadcrumb without spamming the default-info logs
					// when GitHub is briefly down.
					s.logger.Debug("updater scheduler: background check failed",
						"error", err)
				} else {
					// Re-read config after the (potentially slow) network
					// Check so an admin toggle of AutoUpdate or an addition
					// to SkippedVersions during the in-flight check is
					// honored before we launch an apply.
					liveCfg, cfgErr := s.GetConfig(ctx)
					if cfgErr != nil {
						s.logger.Warn("updater scheduler: re-reading config before auto-apply failed",
							"error", cfgErr)
					} else {
						s.maybeAutoApply(ctx, liveCfg, result)
					}
				}
			}

			// Recompute the next interval from the freshly-read config so
			// admin changes take effect on the very next cycle.
			next := time.Duration(DefaultCheckIntervalHours) * schedulerHourUnit
			if cfg.CheckIntervalHours >= MinCheckIntervalHours {
				next = time.Duration(cfg.CheckIntervalHours) * schedulerHourUnit
			}
			interval = next
			resetTimer(interval)
		}
	}
}

// maybeAutoApply triggers Apply() automatically when AutoUpdate is
// enabled and the just-completed Check found a newer release. It is a
// no-op on Docker hosts (orchestration handles updates) and short-
// circuits when the candidate tag is in cfg.SkippedVersions. After a
// successful Apply, the last-auto-applied marker is persisted so the
// Updates tab can surface "last auto-applied: vX.Y.Z" without polling.
//
// Failures are logged but not fatal: the scheduler keeps running and
// will retry on the next Check tick.
func (s *Service) maybeAutoApply(_ context.Context, cfg Config, result CheckResult) {
	// Re-check the full enable chain. The scheduler reloads cfg right
	// before calling us, but an admin may toggle Enabled or AutoCheck
	// off while the network Check is in flight. Bailing on any of the
	// three closes the disable-mid-flight race.
	if !cfg.Enabled || !cfg.AutoCheck || !cfg.AutoUpdate {
		return
	}
	if !result.UpdateAvailable || result.Latest == "" {
		return
	}
	if s.isDocker {
		// Docker path: orchestration handles updates. Log once per
		// auto-apply skip so operators can confirm AutoUpdate is being
		// honored as a no-op rather than silently misbehaving.
		s.logger.Info("updater scheduler: AutoUpdate skipped on Docker host",
			"candidate", result.Latest)
		return
	}
	for _, skip := range cfg.SkippedVersions {
		if skip == result.Latest {
			s.logger.Info("updater scheduler: AutoUpdate skipped (version on skip list)",
				"candidate", result.Latest)
			return
		}
	}
	// Use applyAuto so the success path in runApply persists the
	// last-auto-applied marker only on the swap-confirmed branch.
	// Apply is async; writing the marker here at kickoff time would
	// record an "applied" event for downloads that ultimately failed
	// checksum or extraction.
	if err := s.applyAuto(result.Latest); err != nil { //nolint:contextcheck // applyAuto detaches by design; goroutine must outlive scheduler tick
		// ErrAlreadyRunning / ErrRestartRequired are expected when the
		// admin already triggered a manual Apply. Anything else is a
		// real failure but still non-fatal for the scheduler.
		s.logger.Warn("updater scheduler: AutoUpdate Apply failed",
			"candidate", result.Latest, "error", err)
		return
	}
}

// applyAuto kicks off Apply with the auto-apply marker set on the
// goroutine context, so runApply's success path persists the
// last-auto-applied row. Mirrors Apply's preconditions (Docker block,
// CompareAndSwap, restart-required guard) so a manual Apply already in
// flight does not get clobbered.
//
// Takes no ctx because runApply uses context.Background() (the goroutine
// must outlive the originating tick); the marker write also uses a
// fresh background context for the same reason. Keeping the signature
// ctx-free makes that intent explicit; the contextcheck suppression at
// the call site documents that applyAuto has no ctx parameter by design
// because its internal goroutine (below) must outlive the scheduler tick.
func (s *Service) applyAuto(candidateVersion string) error {
	if s.isDocker {
		return fmt.Errorf("binary update is not supported in Docker environments")
	}
	s.mu.RLock()
	restartRequired := s.restartRequired
	s.mu.RUnlock()
	if restartRequired {
		return ErrRestartRequired
	}
	if !s.applyRunning.CompareAndSwap(0, 1) {
		return ErrAlreadyRunning
	}
	go func() {
		s.runApply(context.Background(), candidateVersion)
		// runApply has already toggled state. Check status to confirm
		// the swap actually succeeded (markRestartRequired was called)
		// and only then persist the auto-apply marker.
		s.mu.RLock()
		restarted := s.restartRequired
		pending := s.pendingVersion
		s.mu.RUnlock()
		if restarted && pending == candidateVersion {
			// The marker write uses a fresh background context so it
			// outlives the originating tick; persistence failure is
			// logged but not surfaced to the scheduler caller (which
			// has already returned).
			if err := s.markAutoApplied(context.Background(), candidateVersion); err != nil {
				s.logger.Warn("updater scheduler: persisting last-auto-applied failed",
					"candidate", candidateVersion, "error", err)
			}
		}
	}()
	return nil
}

// markAutoApplied persists the last-auto-applied timestamp and version.
// Written outside SetConfig because these fields are scheduler-owned
// (not user-editable) and SetConfig's transaction semantics already
// cover the user-facing knobs only.
func (s *Service) markAutoApplied(ctx context.Context, version string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning markAutoApplied tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range []struct{ k, v string }{
		{SettingLastAutoApplied, now},
		{SettingLastAutoAppliedVer, version},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			kv.k, kv.v, now); err != nil {
			return fmt.Errorf("persisting %q: %w", kv.k, err)
		}
	}
	return tx.Commit()
}

// AddSkippedVersion appends a version tag to the persisted skip list.
// Idempotent: a tag already present is a no-op. The scheduler reads
// SkippedVersions on every tick, so the next auto-apply candidate is
// gated on the post-write list without restarting the scheduler.
func (s *Service) AddSkippedVersion(ctx context.Context, version string) error {
	if version == "" {
		return fmt.Errorf("version tag must be non-empty")
	}
	s.skippedVersionsMu.Lock()
	defer s.skippedVersionsMu.Unlock()
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	for _, v := range cfg.SkippedVersions {
		if v == version {
			return nil
		}
	}
	cfg.SkippedVersions = append(cfg.SkippedVersions, version)
	return s.writeSkippedVersions(ctx, cfg.SkippedVersions)
}

// RemoveSkippedVersion removes a tag from the skip list. Idempotent:
// removing a tag that is not present is a no-op.
func (s *Service) RemoveSkippedVersion(ctx context.Context, version string) error {
	s.skippedVersionsMu.Lock()
	defer s.skippedVersionsMu.Unlock()
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	out := make([]string, 0, len(cfg.SkippedVersions))
	for _, v := range cfg.SkippedVersions {
		if v != version {
			out = append(out, v)
		}
	}
	return s.writeSkippedVersions(ctx, out)
}

// ListSkippedVersions returns the current skip list. Convenience
// wrapper around GetConfig for handlers that need only this slice.
func (s *Service) ListSkippedVersions(ctx context.Context) ([]string, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return cfg.SkippedVersions, nil
}

// writeSkippedVersions persists the skip list. An empty list is stored
// as the empty string ("" round-trips through GetConfig as no skips)
// rather than the JSON literal "[]" to keep the settings row absent of
// noise when the user clears every entry.
func (s *Service) writeSkippedVersions(ctx context.Context, list []string) error {
	value := ""
	if len(list) > 0 {
		b, err := json.Marshal(list)
		if err != nil {
			return fmt.Errorf("marshaling skipped versions: %w", err)
		}
		value = string(b)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingSkippedVersions, value, now); err != nil {
		return fmt.Errorf("persisting skipped versions: %w", err)
	}
	return nil
}
