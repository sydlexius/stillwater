// Package conflict detects and gates write-back and round-trip conflicts
// between Stillwater and connected media servers (Emby, Jellyfin, Lidarr).
//
// Background: when Stillwater writes NFO or artwork files into the shared
// library directory and then POSTs metadata to the peer, the peer may itself
// persist that metadata back to disk under its own naming convention,
// producing duplicate files. This package detects the peer configuration
// that causes that behavior and gates Stillwater's own writes until the
// user remediates -- either by fixing the peer manually or by opting in to
// "Let Stillwater manage" which PATCHes the peer to disable its savers.
package conflict

import (
	"encoding/json"
	"time"
)

// Axis identifies which kind of write is at risk on a peer.
//
//   - AxisImage: the peer writes image files to disk (e.g., backdrop.jpg,
//     fanart.jpg) that would collide with Stillwater's writes.
//   - AxisNFO: the peer writes NFO XML to disk (artist.nfo) that would
//     overwrite Stillwater's NFO.
//   - AxisRoundTrip: two or more enabled connections' library paths overlap,
//     so any Stillwater write reaches a second peer via shared disk even if
//     only one peer has a saver enabled. Treated as both image and NFO risk.
type Axis string

// Axis values. Stored as JSON strings in the API surface.
const (
	AxisImage     Axis = "image"
	AxisNFO       Axis = "nfo"
	AxisRoundTrip Axis = "round_trip"
)

// ConnectionState captures what we know about one connection's write-back
// behavior. All fields are safe to surface in the UI.
type ConnectionState struct {
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name"`
	ConnectionType string `json:"connection_type"`
	// Enabled mirrors connections.enabled at the time of detection. Disabled
	// connections do not contribute to the gate but are still surfaced so
	// the settings page can show "Detected on this server" values for every
	// connection the user has configured.
	Enabled bool `json:"enabled"`
	// ManageServerFiles mirrors the per-connection "Let Stillwater manage
	// images and NFO files on this server" toggle. When true the peer's
	// savers have been patched off and its write-back signals are
	// suppressed from the gate.
	ManageServerFiles bool `json:"manage_server_files"`
	// NFOWriteback is true when the peer is configured to persist NFO files
	// to disk. Populated from the platform-specific Check*NFO helpers.
	NFOWriteback bool `json:"nfo_writeback"`
	// ImageWriteback is true when the peer is configured to persist image
	// files to disk.
	ImageWriteback bool `json:"image_writeback"`
	// LibraryName is the first music library on the peer that had a saver
	// enabled, populated for UI context. Empty for Lidarr (global setting)
	// or when no saver was found.
	LibraryName string `json:"library_name,omitempty"`
	// Paths is the list of filesystem roots reported by the peer for its
	// music libraries, used for round-trip detection.
	Paths []string `json:"paths,omitempty"`
	// CheckErr records the most recent detection error for this connection,
	// if any. A non-empty string means the state is provisional; callers
	// should treat unknown as "assume conflict" and display a diagnostic
	// warning rather than silently passing.
	CheckErr string `json:"check_err,omitempty"`
	// CheckedAt is when the state above was last refreshed.
	CheckedAt time.Time `json:"checked_at"`
}

// HasAnyConflict returns true if this connection contributes to the global
// gate. A disabled connection never contributes regardless of saver state;
// a managed connection has its savers disabled by Stillwater so it does
// not contribute either. For every other connection, either saver axis
// being on is enough.
func (s ConnectionState) HasAnyConflict() bool {
	if !s.Enabled || s.ManageServerFiles {
		return false
	}
	return s.NFOWriteback || s.ImageWriteback
}

// RoundTrip describes two connections whose library roots overlap on disk,
// so any Stillwater write will reach both peers through the shared filesystem.
type RoundTrip struct {
	// ConnectionAID and ConnectionBID are the two connections whose paths
	// overlap. Surface both in the UI so the user can identify which peer
	// to reconfigure.
	ConnectionAID   string `json:"connection_a_id"`
	ConnectionAName string `json:"connection_a_name"`
	ConnectionBID   string `json:"connection_b_id"`
	ConnectionBName string `json:"connection_b_name"`
	// OverlappingPath is the common ancestor segment (or equal path) that
	// makes the two peers share disk state. Shown as a bullet in banner
	// state C so the user can see what exactly overlapped.
	OverlappingPath string `json:"overlapping_path"`
}

// ForeignFileSummary surfaces the foreign-file ledger size on the global
// conflict ledger so the banner handler can render the slate/blue warning
// state in one place. The detector does not write this field; it is
// populated by the banner handler from the foreign repository before the
// banner template is rendered. Kept on the same view-side struct so the
// banner has a single source of truth across all five states.
type ForeignFileSummary struct {
	// Count is the number of unallowlisted foreign-file rows currently in
	// the foreign_files table. Zero suppresses the warning state.
	Count int `json:"count"`
}

// Ledger is the aggregated conflict snapshot for every enabled connection
// plus any round-trip pairings. Produced by Detector.Refresh / Current and
// consumed by Gate (for enforcement) and the banner handler (for UI).
type Ledger struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Connections []ConnectionState `json:"connections"`
	RoundTrips  []RoundTrip       `json:"round_trips,omitempty"`
	// ForeignFiles is populated by the banner handler (not by the
	// connection detector) with the current foreign-file count. A non-zero
	// Count enables the slate/blue warning state in BannerState only when
	// no other (image/nfo/round-trip) state is present.
	ForeignFiles ForeignFileSummary `json:"foreign_files,omitempty"`
}

// AnyImageConflict reports whether image writes should be gated globally.
// True when any enabled, unmanaged connection has image_writeback=true,
// any round-trip pairing exists, or any enabled unmanaged connection has
// a non-empty CheckErr (saver state unknown -- fail closed per the
// ConnectionState.CheckErr contract).
func (l Ledger) AnyImageConflict() bool {
	if len(l.RoundTrips) > 0 {
		return true
	}
	for i := range l.Connections {
		c := &l.Connections[i]
		if !c.Enabled {
			continue
		}
		if c.ManageServerFiles {
			continue
		}
		if c.CheckErr != "" {
			return true
		}
		if c.ImageWriteback {
			return true
		}
	}
	return false
}

// AnyNFOConflict reports whether NFO writes should be gated globally. Mirror
// of AnyImageConflict for the NFO axis, including the same fail-closed
// treatment of CheckErr.
func (l Ledger) AnyNFOConflict() bool {
	if len(l.RoundTrips) > 0 {
		return true
	}
	for i := range l.Connections {
		c := &l.Connections[i]
		if !c.Enabled {
			continue
		}
		if c.ManageServerFiles {
			continue
		}
		if c.CheckErr != "" {
			return true
		}
		if c.NFOWriteback {
			return true
		}
	}
	return false
}

// BannerState derives the UI banner state from the ledger. States:
//
//   - "round_trip" (state C, red): round-trip pairings exist; shows
//     overlapping path.
//   - "image_only" (state A, amber): image writeback detected but no NFO
//     writeback.
//   - "nfo_only" (state B, amber): NFO writeback detected but no image
//     writeback.
//   - "both" (state A+B composite, amber): both image and NFO writeback.
//     Rendered as the image-axis variant with NFO noted.
//   - "foreign_files" (state E, slate/blue): no configuration conflict, but
//     unallowlisted foreign image files were detected by the foreign-file
//     scanner (#1185). Deliberately NOT amber/red because this is a
//     warning, not a configuration alarm: nothing is gated, the user is
//     simply being told that media-server-named files exist without
//     Stillwater provenance.
//   - "clean" (state D, emerald): no conflicts and no foreign files.
//
// Precedence is highest-severity-wins: round_trip > image/nfo write-back >
// foreign_files > clean. Foreign-file warnings are intentionally suppressed
// while a real conflict is active so the user is not shown two banners at
// once; the foreign-files page remains reachable from the settings sidebar.
//
// Callers use this to pick the banner variant; the gate decisions
// (AnyImageConflict / AnyNFOConflict) remain independent.
func (l Ledger) BannerState() string {
	if len(l.RoundTrips) > 0 {
		return "round_trip"
	}
	img := l.AnyImageConflict()
	nfo := l.AnyNFOConflict()
	switch {
	case img && nfo:
		return "both"
	case img:
		return "image_only"
	case nfo:
		return "nfo_only"
	}
	if l.ForeignFiles.Count > 0 {
		return "foreign_files"
	}
	return "clean"
}

// Marshal returns a JSON encoding of the ledger for wire transport.
func (l Ledger) Marshal() ([]byte, error) {
	return json.Marshal(l)
}
