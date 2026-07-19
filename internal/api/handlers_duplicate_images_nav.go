// Package api -- handlers_duplicate_images_nav.go
//
// The sidebar's "Images" section (#2608).
//
// GET /api/v1/reports/duplicate-images/nav
//
// THE LOAD-BEARING PROPERTY OF THIS FILE: the request path NEVER SCANS.
// It reads a cached snapshot (dupimages.Cache.Get, an O(1) struct copy) plus
// one cheap indexed COUNT for the unmatched half, and renders them. It does
// not call ScanFanartDuplicates, does not call ScanPlatformBackdropDuplicates,
// and does not touch the filesystem or any connected platform. That is the
// whole reason the duplicate counts are cached: the library scan re-hashes
// every artist's fanart FROM DISK and the platform scan queries every
// connected Emby/Jellyfin for every artist. Either one on a 60s sidebar poll
// would be catastrophic, which is why the pre-#2608 nav links carried no count
// pill at all.
//
// The cache is filled out-of-band by:
//   - maintenance.StartDuplicateImageCountRefresh (periodic, 12h default), and
//   - Cache.TriggerRefresh, a fire-and-forget background kick issued by this
//     handler ONLY when the cache has never been computed. That is the "lazy"
//     path from the issue: it returns the current (empty) snapshot immediately
//     and lets the scan land in the background, so the render is never blocked.
//     Single-flight, so a burst of sidebar polls produces one scan.
//
// SCOPE -- this endpoint serves the WHOLE section: the header, the Unmatched
// row and the duplicate rows. It is deliberately not split with a
// server-rendered wrapper, because the section HIDES ENTIRELY when all three
// counts are zero and that decision needs all three at once. An empty body
// therefore means "render no section", and the container in sidebar.templ
// collapses to nothing.
//
// Response contract:
//   - empty body when every count is zero (no header, no rows);
//   - the section otherwise, carrying only the rows whose own count is > 0:
//     Unmatched, Library Duplicates, then one "<Platform> Duplicates" row per
//     offending platform type.
//
// Admin-only, like every other report count endpoint (403 JSON envelope; the
// sidebar only hydrates this section for administrators, so a non-admin never
// polls it, and a non-admin session renders no container at all).
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"unicode"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/dupimages"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/web/templates"
)

// dupImageCache returns the router's duplicate-image count cache, installing
// this router's scan sources on first use.
//
// The sources are installed here rather than in cmd/stillwater/main.go because
// the router already holds the handles the scans need (r.pipeline, r.publisher,
// r.connectionService) and the cache's other consumer (the maintenance
// scheduler) needs only the cache itself.
func (r *Router) dupImageCache() *dupimages.Cache {
	cache := dupimages.Shared()
	r.dupImageOnce.Do(func() {
		cache.SetLogger(r.logger)
		cache.SetSources(r.libraryDupCount, r.platformDupCounts)
	})
	return cache
}

// libraryDupCount runs the EXPENSIVE local scan. Called only from the
// background refresh path, never from a request handler.
func (r *Router) libraryDupCount(ctx context.Context) (int, error) {
	repairer, ok := r.pipeline.(fanartDuplicateRepairer)
	if !ok {
		// Fail loud: the production pipeline always implements this. A silent
		// zero here would render the library row as permanently clean.
		r.logger.Error("pipeline does not implement fanartDuplicateRepairer; library duplicate-image count unavailable")
		return 0, errLibraryDupScanUnavailable
	}
	report, err := repairer.ScanFanartDuplicates(ctx)
	if err != nil {
		return 0, err
	}
	if report.ScanErrors > 0 {
		// PARTIAL scan -- report.ScanErrors artists could not be re-hashed (a
		// dropped NFS/SMB mount is the ordinary cause) and were SKIPPED, yet
		// ScanFanartDuplicates still returns err == nil. ExactRedundantSlots
		// therefore covers only the reachable half of the library: a confident
		// UNDERCOUNT the operator has no way to distinguish from the truth.
		//
		// Surface it as an error so Refresh carries the last known value
		// forward instead of caching this number as fact (#2608).
		r.logger.Warn("library duplicate-image count scan was partial; not caching the count",
			slog.Int("scan_errors", report.ScanErrors),
			slog.Int("scanned_redundant_slots", report.ExactRedundantSlots))
		return 0, fmt.Errorf("library duplicate-image scan skipped %d artists: %w",
			report.ScanErrors, dupimages.ErrPartialScan)
	}
	// Count semantic: redundant IMAGE slots, not affected artists. The rows
	// count images.
	return report.ExactRedundantSlots, nil
}

// platformDupCounts runs the EXPENSIVE platform sweep and buckets the result
// by platform TYPE. Called only from the background refresh path.
//
// Bucketing by type rather than by connection is deliberate: the row reads
// "Emby Duplicates", so two Emby connections that both carry duplicates
// collapse into ONE row with the combined count instead of two rows the
// operator has to add up, and a connection the user named "Living Room Emby"
// still reads "Emby Duplicates".
func (r *Router) platformDupCounts(ctx context.Context) ([]dupimages.PlatformCount, error) {
	if r.publisher == nil {
		// Fail loud: the production router always wires a Publisher.
		r.logger.Error("publisher not wired; platform duplicate-image count unavailable")
		return nil, errPlatformDupScanUnavailable
	}
	return r.platformDupCountsFrom(ctx, r.publisher)
}

// platformBackdropDupScanner is the single publisher capability
// platformDupCounts needs. Narrowed to an interface so the partial-sweep guard
// below is testable without standing up a live Emby -- the same reason
// fanartDuplicateRepairer narrows the pipeline, and the same split as
// bucketByPlatformType's testable free-function form.
type platformBackdropDupScanner interface {
	ScanPlatformBackdropDuplicates(ctx context.Context) (publish.PlatformBackdropDupReport, error)
}

// platformDupCountsFrom is platformDupCounts against an explicit scanner.
func (r *Router) platformDupCountsFrom(ctx context.Context, scanner platformBackdropDupScanner) ([]dupimages.PlatformCount, error) {
	report, err := scanner.ScanPlatformBackdropDuplicates(ctx)
	if err != nil {
		return nil, err
	}
	if report.ScanErrors > 0 {
		// PARTIAL sweep -- see libraryDupCount for the shape. This half's
		// failure mode is WORSE than an undercount: when a platform is
		// unreachable EVERY per-artist query fails, so PerArtist comes back
		// empty with err == nil. Bucketing that yields an empty slice, which
		// Refresh reads as the legitimate "every connected platform is clean"
		// answer and uses to CLEAR the platform rows -- erasing a real, still
		// present duplicate count during a transient outage and reporting
		// success in the log while doing it (#2608).
		r.logger.Warn("platform duplicate-image count sweep was partial; not caching the counts",
			slog.Int("scan_errors", report.ScanErrors),
			slog.Int("scanned_rows", len(report.PerArtist)))
		return nil, fmt.Errorf("platform duplicate-image sweep skipped %d artist/connection scans: %w",
			report.ScanErrors, dupimages.ErrPartialScan)
	}
	return r.bucketByPlatformType(ctx, report), nil
}

// bucketByPlatformType groups a platform scan's rows by connection type using
// the router's connection service.
func (r *Router) bucketByPlatformType(ctx context.Context, report publish.PlatformBackdropDupReport) []dupimages.PlatformCount {
	if r.connectionService == nil {
		// Fail loud: without the connection service a row cannot be attributed
		// to a platform, and an unattributed row must not be invented.
		r.logger.Error("connection service not wired; cannot label platform duplicate rows")
		return nil
	}
	resolve := func(connID string) (string, error) {
		conn, err := r.connectionService.GetByID(ctx, connID)
		if err != nil {
			return "", err
		}
		if conn == nil {
			return "", errConnectionNotFound
		}
		return conn.Type, nil
	}
	return bucketByPlatformType(report, resolve, r.logger)
}

// bucketByPlatformType sums a platform scan's redundant counts per connection
// TYPE. Split from the Router method so it can be tested against a plain
// resolver rather than a live *connection.Service.
//
// A connection whose type cannot be resolved is SKIPPED with a warning rather
// than bucketed under a guess: a row is a claim about WHICH platform is dirty,
// and mislabeling it sends the operator to the wrong place. The scan itself
// only visits enabled, status-ok connections, so "actually connected" is
// already established by the time a row reaches here.
func bucketByPlatformType(
	report publish.PlatformBackdropDupReport,
	resolveType func(connID string) (string, error),
	logger *slog.Logger,
) []dupimages.PlatformCount {
	totals := make(map[string]int, 2)
	// Memoize lookups: PerArtist carries one row per artist/connection pair, so
	// the same connection id recurs once per affected artist.
	typeOf := make(map[string]string, 4)

	for _, d := range report.PerArtist {
		if d.Redundant <= 0 {
			continue
		}
		connType, seen := typeOf[d.ConnectionID]
		if !seen {
			resolved, err := resolveType(d.ConnectionID)
			if err != nil {
				logger.Warn("platform duplicate row skipped: connection type unresolved",
					"connection_id", d.ConnectionID, "error", err)
				resolved = "" // cached below so one failure is logged once
			}
			connType = resolved
			typeOf[d.ConnectionID] = connType
		}
		if connType == "" {
			continue
		}
		totals[connType] += d.Redundant
	}

	out := make([]dupimages.PlatformCount, 0, len(totals))
	for connType, count := range totals {
		out = append(out, dupimages.PlatformCount{
			Type:  connType,
			Label: platformDisplayName(connType),
			Count: count,
		})
	}
	// Stable order so the sidebar does not reshuffle between polls (Go map
	// iteration order is randomized).
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// errConnectionNotFound marks a scan row whose connection no longer resolves.
var errConnectionNotFound = wiringError("connection not found")

// platformDisplayName maps a connection type key to its display name.
//
// The known types are listed only to pin their exact brand casing. An unknown
// type is TITLE-CASED and returned, never dropped: the row label is composed
// generically as "<Name> Duplicates", so a platform type added later renders a
// correct-looking row ("Plex Duplicates") the day it ships, without an edit
// here. Dropping it instead would make a genuinely dirty platform silently
// invisible, which is the worse failure -- the operator would never learn there
// were duplicates to clean.
func platformDisplayName(connType string) string {
	switch connType {
	case connection.TypeEmby:
		return "Emby"
	case connection.TypeJellyfin:
		return "Jellyfin"
	case connection.TypeLidarr:
		return "Lidarr"
	case "":
		return ""
	default:
		// Title-case the raw key so an unmapped type still reads as a name
		// rather than a config token ("plex" -> "Plex").
		r := []rune(connType)
		return string(unicode.ToUpper(r[0])) + string(r[1:])
	}
}

// handleDuplicateImagesNav serves the Images section. See the file comment:
// this handler performs no scan.
func (r *Router) handleDuplicateImagesNav(w http.ResponseWriter, req *http.Request) {
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "administrator role required",
		})
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	cache := r.dupImageCache()
	counts := cache.Get() // O(1). No scan.

	if !counts.Computed {
		// Cold cache: ask for numbers in the background and answer NOW with
		// what we have (nothing). The scan does not run on this goroutine and
		// this request does not wait for it.
		cache.TriggerRefresh()
	}

	// Cheap indexed COUNT against the foreign-file repo -- the same call the
	// conflict banner already makes on every poll. Not part of the cached
	// snapshot because it is not a scan.
	//
	// ACCEPTED FAILURE MODE (decided by the maintainer, #2608). If this count
	// FAILS -- a DB lock, a closed DB, a migration window -- foreignSummaryForBanner
	// logs a Warn and returns 0. With the duplicate counts also zero, the view
	// is Empty, the body is empty, and the ENTIRE Images section disappears:
	// indistinguishable, on screen, from "everything is clean".
	//
	// This is the direct consequence of the hide-when-zero spec, not an
	// oversight. Hiding the section at a zero count necessarily means hiding it
	// when the count cannot be determined, because the two states produce the
	// same number here. The alternative -- an error state, a banner, or a retry
	// -- was considered and rejected: it puts infrastructure noise into the
	// nav on every transient DB blip.
	//
	// The Warn from foreignSummaryForBanner is therefore the ONLY operator
	// signal that the section vanished because of a failure rather than because
	// the library is clean. Pinned by
	// TestDupImagesNav_UnmatchedCountFailureHidesSectionAndWarns.
	unmatched := r.foreignSummaryForBanner(req.Context())

	view := r.buildImagesNavView(req, counts, unmatched)

	w.WriteHeader(http.StatusOK)
	if view.Empty() {
		// Empty body -> the hydration container renders nothing at all: no
		// header, no rows. This is the "everything is clean" steady state.
		return
	}

	renderTempl(w, req, templates.ImagesNav(view))
}

// buildImagesNavView maps a cached snapshot plus the unmatched count onto the
// fragment's view model.
//
// Visible labels stay terse ("Unmatched", "Library Duplicates", "Emby
// Duplicates") so they cannot truncate -- the truncation of "Platform Backdrop
// Duplicates" is what this issue exists to fix. The descriptive, count-bearing
// name goes on aria-label, interpolated fmt-style (same convention as
// nav.reports.foreign.aria).
func (r *Router) buildImagesNavView(req *http.Request, counts dupimages.Counts, unmatched int) templates.ImagesNavView {
	tr := i18n.TFromCtx(req.Context())

	view := templates.ImagesNavView{
		BasePath:       r.basePath,
		SectionLabel:   tr.T("nav.images"),
		UnmatchedCount: unmatched,
		UnmatchedLabel: tr.T("nav.images.unmatched"),
		LibraryCount:   counts.Library,
		LibraryLabel:   tr.T("nav.images.library_duplicates"),
	}
	if view.UnmatchedCount > 0 {
		view.UnmatchedAria = interpolate(tr.T("nav.images.unmatched.aria"),
			"nav.images.unmatched.aria", view.UnmatchedCount)
	}
	if view.LibraryCount > 0 {
		view.LibraryAria = interpolate(tr.T("nav.images.library_duplicates.aria"),
			"nav.images.library_duplicates.aria", view.LibraryCount)
	}

	for _, p := range counts.Platforms {
		view.Platforms = append(view.Platforms, templates.ImagesNavPlatformRow{
			Type:  p.Type,
			Label: interpolate(tr.T("nav.images.platform_duplicates"), "nav.images.platform_duplicates", p.Label),
			Aria:  interpolate(tr.T("nav.images.platform_duplicates.aria"), "nav.images.platform_duplicates.aria", p.Count, p.Label),
			Count: p.Count,
		})
	}
	return view
}

// interpolate applies fmt-style args to a translated template, falling back to
// the raw template when the key is missing (i18n.T echoes the key on a miss, so
// formatting it would emit "%!d(MISSING)" garbage into the accessible name).
func interpolate(tmpl, key string, args ...any) string {
	if tmpl == key {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

// storeLibraryDupCount records a library count the caller already computed,
// leaving the platform rows untouched. Called by the local
// backdrop-duplicates report page, which pays for the full scan anyway (#2608).
// It stamps the library half's provenance, which is what makes a periodic
// Refresh that started BEFORE this store decline to overwrite it on the way out
// (the scans are minutes long, so a remediation + report-page visit landing
// mid-scan is ordinary, not exotic).
func (r *Router) storeLibraryDupCount(count int) {
	r.dupImageCache().StoreLibrary(count)
}

// storePlatformDupCounts records per-platform counts the caller already
// computed, leaving the library count untouched. Called by the platform
// backdrop-duplicates report page.
// Same provenance-stamping rationale as storeLibraryDupCount.
func (r *Router) storePlatformDupCounts(platforms []dupimages.PlatformCount) {
	r.dupImageCache().StorePlatforms(platforms)
}

// Sentinel errors for unwired dependencies. Distinct values so a wiring bug is
// never mistaken for a scan failure.
var (
	errLibraryDupScanUnavailable  = wiringError("library duplicate-image scan unavailable: pipeline does not implement fanartDuplicateRepairer")
	errPlatformDupScanUnavailable = wiringError("platform duplicate-image scan unavailable: publisher not wired")
)

type wiringError string

func (e wiringError) Error() string { return string(e) }
