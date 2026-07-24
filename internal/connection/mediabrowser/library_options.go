// Package mediabrowser holds the conflict-detection helpers shared between
// the Emby and Jellyfin clients. Both servers descend from the same
// MediaBrowser code lineage and expose the identical
// /Library/VirtualFolders surface for managing per-library saver settings,
// so the snapshot/disable/restore flow is byte-for-byte the same. Per
// platform clients delegate to these helpers and contribute only the
// typed adapters their REST surface differs on (e.g. typed
// GetMusicLibraries variants whose VirtualFolder shape differs slightly).
package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Transport abstracts the per-client HTTP plumbing the helpers need.
// emby.Client and jellyfin.Client satisfy this implicitly via the Get,
// GetRaw, PostJSON, and Do methods they inherit from httpclient.BaseClient.
//
// Do is the one raw primitive in this set: the image upload/delete free
// functions in image_writers.go build a base64-encoded plain-text POST body
// (not JSON) and a bodyless DELETE, neither of which fits Get/GetRaw/
// PostJSON's contracts. It returns the unconsumed *http.Response so callers
// keep full control of status-code handling and body draining, matching
// what the hand-rolled per-package methods this replaces did directly.
type Transport interface {
	Get(ctx context.Context, path string, result any) error
	GetRaw(ctx context.Context, path string) ([]byte, string, error)
	PostJSON(ctx context.Context, path string, body io.Reader, result any) error
	Do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error)
}

// LibraryWriteBackSnapshot is the persisted form of a peer's
// pre-Stillwater saver configuration. Stored on the connection row so
// opt-out can replay the original state. Version bumps if the shape
// evolves; restore refuses unknown versions to avoid misapplying old
// snapshots after a future schema change.
type LibraryWriteBackSnapshot struct {
	Version       int                         `json:"version"`
	SnapshottedAt time.Time                   `json:"snapshotted_at"`
	Libraries     []LibrarySaverSnapshotEntry `json:"libraries"`
}

// LibrarySaverSnapshotEntry holds one library's saver state at snapshot
// time. LibraryName is informational only (UI rendering); LibraryID is
// the authoritative key used during restore.
type LibrarySaverSnapshotEntry struct {
	LibraryID         string   `json:"library_id"`
	LibraryName       string   `json:"library_name"`
	SaveLocalMetadata bool     `json:"save_local_metadata"`
	MetadataSavers    []string `json:"metadata_savers"`
}

// RawMusicLibrary is the lossless shape DisableFileWriteBack and
// RestoreLibraryOptions thread through. Options is the library's full
// LibraryOptions JSON object from the peer; preserving every field our
// Go struct does not model is what keeps the peer from
// NullReferenceException-ing on a partial PATCH.
type RawMusicLibrary struct {
	ID      string
	Name    string
	Options map[string]any
}

// SanitizeLibraryOptions drops keys whose values are null in the raw map.
// The peer's POST handler treats some fields as non-nullable and throws
// when they arrive as explicit nulls (the GET response happily returns
// them that way). Dropping null keys before serialization lets the peer
// fill in defaults for those fields instead of crashing on them.
func SanitizeLibraryOptions(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// BuildSnapshot wraps a slice of per-library entries in the versioned
// envelope and serializes it. Per-client SnapshotLibraryOptions methods
// stay in their packages because they call typed GetMusicLibraries
// variants whose VirtualFolder shapes differ; they convert their typed
// entries to LibrarySaverSnapshotEntry slices and pass them here for
// version stamping and JSON encoding.
func BuildSnapshot(entries []LibrarySaverSnapshotEntry) (string, error) {
	snap := LibraryWriteBackSnapshot{
		Version:       1,
		SnapshottedAt: time.Now().UTC(),
		Libraries:     entries,
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("encoding snapshot: %w", err)
	}
	return string(buf), nil
}

// GetMusicLibrariesRaw fetches /Library/VirtualFolders as an array of
// arbitrary JSON objects and returns each music library's ItemId + Name +
// full LibraryOptions map. A library counts as "music" if its
// CollectionType is explicitly "music" (case-insensitive) OR is empty
// (some installs leave it blank for mixed/legacy libraries). Every
// candidate is logged at debug so users can verify what the conflict
// detector is considering.
//
// The platform string is a short identifier ("emby" / "jellyfin")
// embedded in the debug log lines so operators can distinguish per-server
// traces in a multi-connection setup.
func GetMusicLibrariesRaw(ctx context.Context, t Transport, logger *slog.Logger, platform string) ([]RawMusicLibrary, error) {
	var folders []map[string]any
	if err := t.Get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}
	var out []RawMusicLibrary
	for _, f := range folders {
		collectionType, _ := f["CollectionType"].(string)
		name, _ := f["Name"].(string)
		id, _ := f["ItemId"].(string)
		locs, _ := f["Locations"].([]any)
		paths := make([]string, 0, len(locs))
		for _, v := range locs {
			if s, ok := v.(string); ok {
				paths = append(paths, s)
			}
		}
		ct := strings.TrimSpace(strings.ToLower(collectionType))
		include := ct == "music" || ct == ""
		if logger != nil {
			logger.Debug(platform+" virtual folder discovered",
				"name", name,
				"collection_type", collectionType,
				"paths", paths,
				"included_as_music", include,
			)
		}
		if !include {
			continue
		}
		opts, _ := f["LibraryOptions"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		out = append(out, RawMusicLibrary{ID: id, Name: name, Options: opts})
	}
	return out, nil
}

// PostLibraryOptionsRaw wraps opts in the LibraryOptionsInfo envelope and
// POSTs to /Library/VirtualFolders/LibraryOptions. The endpoint refuses
// a bare LibraryOptions body with an opaque 500 ("Object reference not
// set to an instance of an object."). Empirically (verified against Emby
// 4.x via a throwaway diagnostic) it requires the wrapper
// {"Id": <libraryID>, "LibraryOptions": {...}}. The inner map must
// include every field the peer originally returned; omitted fields are
// silently dropped because the endpoint performs a full REPLACE on
// LibraryOptions rather than a merge.
//
// Callers are expected to pass the full options map from a GET, mutate
// only the fields they mean to change, and let this helper wrap and
// POST. The logged body preserves diagnostic value when future
// peer-version drift breaks the endpoint.
func PostLibraryOptionsRaw(ctx context.Context, t Transport, logger *slog.Logger, platform, libraryID string, opts map[string]any) error {
	wrapper := map[string]any{
		"Id":             libraryID,
		"LibraryOptions": opts,
	}
	body, err := json.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}
	if logger != nil {
		logger.Debug(platform+" library options POST", "library_id", libraryID, "body", string(body))
	}
	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return t.PostJSON(ctx, path, bytes.NewReader(body), nil)
}

// CheckImageSaverEnabledRaw reports whether any library in libs has its
// SaveLocalMetadata flag set, returning that library's name. Generic over T
// because the Emby and Jellyfin typed VirtualFolder DTOs are separate types;
// getSaveLocalMetadata/getName read the two fields the check needs out of
// either one. Byte-identical CheckImageSaverEnabled body on both platforms --
// see emby.Client.CheckImageSaverEnabled / jellyfin.Client.CheckImageSaverEnabled.
func CheckImageSaverEnabledRaw[T any](libs []T, getSaveLocalMetadata func(T) bool, getName func(T) string) (bool, string) {
	for i := range libs {
		if getSaveLocalMetadata(libs[i]) {
			return true, getName(libs[i])
		}
	}
	return false, ""
}

// SnapshotLibraryOptionsRaw builds the versioned snapshot envelope from a
// caller's typed library slice. Generic over T for the same reason as
// CheckImageSaverEnabledRaw; nil MetadataSavers is normalized to an empty
// slice so restore always has a concrete list to work with. Byte-identical
// SnapshotLibraryOptions body on both platforms -- see
// emby.Client.SnapshotLibraryOptions / jellyfin.Client.SnapshotLibraryOptions.
func SnapshotLibraryOptionsRaw[T any](libs []T, getItemID, getName func(T) string, getSaveLocalMetadata func(T) bool, getMetadataSavers func(T) []string) (string, error) {
	entries := make([]LibrarySaverSnapshotEntry, 0, len(libs))
	for i := range libs {
		savers := getMetadataSavers(libs[i])
		if savers == nil {
			savers = []string{}
		}
		entries = append(entries, LibrarySaverSnapshotEntry{
			LibraryID:         getItemID(libs[i]),
			LibraryName:       getName(libs[i]),
			SaveLocalMetadata: getSaveLocalMetadata(libs[i]),
			MetadataSavers:    savers,
		})
	}
	return BuildSnapshot(entries)
}

// DisableFileWriteBack disarms a peer's local-metadata writers on every
// music library via a lossless raw-JSON round-trip. The peer's
// LibraryOptions response carries many fields our Go struct doesn't model;
// PATCHing only the modeled subset drops the rest and makes the server
// crash. So we GET each library's full options as a raw JSON map, mutate
// only the two keys below, and POST the merged map back.
//
// BOTH keys are load-bearing. SaveLocalMetadata=false is NOT a master
// switch, despite what this comment used to claim:
//
//	Emby 4.9.5.0 and Jellyfin 10.11.10, SaveLocalMetadata=false and
//	MetadataSavers=["Nfo"]  ->  the peer STILL writes artist.nfo into the
//	library. Clearing MetadataSavers is what actually stops it.
//
// That was not a theory. With the saver left armed, renaming an artist
// directory let the peer RE-CREATE the directory it had just been renamed
// away from (writing a lone artist.nfo into it), the scanner re-imported
// it, and a duplicate artist appeared -- the #2380 resurrection, whose
// engine this is. Clearing MetadataSavers stopped it, on the same fixture,
// same binary (#2420).
//
// The old comment also warned that mutating MetadataSavers crashed the
// peer with a NullReferenceException. That predates the raw round-trip
// this function now performs: the crash came from POSTing a modeled
// subset and dropping fields the server needed (see the paragraph above),
// which SanitizeLibraryOptions + the full-map POST already fixed.
// Re-verified against both live peers: POSTing MetadataSavers=[] returns
// 204, reads back empty, and leaves the server healthy.
//
// The saver list is restored from the snapshot on the way out
// (RestoreLibraryOptions), so clearing it here is reversible ON THE HAPPY PATH.
// Two pre-existing holes in the surrounding toggle mean that is not an absolute
// guarantee, and clearing a LIST rather than a bool raises their stakes -- a
// user can re-tick a checkbox they lost, but cannot recover a saver list they
// never knew they had:
//
//   - Re-enabling after a failed disable re-snapshots the peer in its ALREADY
//     CLEARED state, overwriting the good snapshot (#2422).
//   - RestoreLibraryOptions only replays libraries present in the snapshot, so a
//     music library ADDED while the toggle was on gets disarmed and never
//     restored (#2423).
//
// Both are tracked; neither is introduced here.
func DisableFileWriteBack(ctx context.Context, t Transport, logger *slog.Logger, platform string) error {
	libs, err := GetMusicLibrariesRaw(ctx, t, logger, platform)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}
	var firstErr error
	for _, lib := range libs {
		opts := SanitizeLibraryOptions(lib.Options)
		opts["SaveLocalMetadata"] = false
		// Marshals to [] rather than null. A null saver list is one of the
		// shapes the peer rejects.
		opts["MetadataSavers"] = []string{}
		if err := PostLibraryOptionsRaw(ctx, t, logger, platform, lib.ID, opts); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if logger != nil {
				logger.Warn("disabling file write-back failed for library", "library", lib.Name, "error", err)
			}
		}
	}
	return firstErr
}

// RestoreLibraryOptions applies a previously saved snapshot to the peer.
// For each library in the snapshot it GETs the current options as raw
// JSON, overlays SaveLocalMetadata + MetadataSavers, and POSTs back.
// Sanitized before the POST to avoid the same null-key crash
// DisableFileWriteBack works around -- the server's POST handler chokes
// on explicit-null values that the GET response happily returns.
func RestoreLibraryOptions(ctx context.Context, t Transport, logger *slog.Logger, platform, snapshotJSON string) error {
	var snap LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return fmt.Errorf("decoding snapshot: %w", err)
	}
	if snap.Version != 1 {
		return fmt.Errorf("unsupported snapshot version %d", snap.Version)
	}
	libs, err := GetMusicLibrariesRaw(ctx, t, logger, platform)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}
	byID := make(map[string]RawMusicLibrary, len(libs))
	for _, lib := range libs {
		byID[lib.ID] = lib
	}
	var firstErr error
	for _, entry := range snap.Libraries {
		lib, ok := byID[entry.LibraryID]
		if !ok {
			if logger != nil {
				logger.Warn("snapshot library missing on peer; skipping",
					"library_id", entry.LibraryID,
					"library_name", entry.LibraryName,
				)
			}
			continue
		}
		opts := SanitizeLibraryOptions(lib.Options)
		opts["SaveLocalMetadata"] = entry.SaveLocalMetadata
		savers := entry.MetadataSavers
		if savers == nil {
			savers = []string{}
		}
		opts["MetadataSavers"] = savers
		if err := PostLibraryOptionsRaw(ctx, t, logger, platform, lib.ID, opts); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if logger != nil {
				logger.Warn("restoring library options failed", "library", lib.Name, "error", err)
			}
		}
	}
	return firstErr
}
