package jellyfin

import (
	"context"
	"fmt"
	"net/url"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
)

// PushMetadata updates metadata for an artist item on the Jellyfin server.
//
// Jellyfin's POST /Items/{id} requires the full item body; partial updates
// with only the changed fields return 400. This method fetches the current
// item, merges Stillwater's fields into it, strips read-only fields that
// Jellyfin rejects, and POSTs the complete body back.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	// Fetch the current item from Jellyfin to get the full body.
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching current item for merge: %w", err)
	}

	// Merge Stillwater's fields into the existing item. Empty values are
	// written unconditionally so that field clears in Stillwater propagate
	// to Jellyfin (the fetch-merge pattern preserves all other fields).
	existing["Name"] = data.Name
	existing["Overview"] = data.Biography
	existing["ForcedSortName"] = data.SortName
	// Jellyfin does not support per-field metadata locks (only a whole-item
	// LockData boolean). Its MetadataField enum lacks "SortName" entirely,
	// so attempting to lock SortName via LockedFields returns HTTP 400 and
	// fails the whole push. ForcedSortName persists across metadata refresh
	// on its own without a lock, so data.LockSortName is intentionally
	// ignored on the Jellyfin path -- the field is consumed only by the
	// Emby push, where the platform documents the lock requirement.
	existing["Genres"] = append([]string{}, data.Genres...)

	// Styles and Moods map to Tags (flat string array on Jellyfin).
	tags := append([]string{}, data.Styles...)
	tags = append(tags, data.Moods...)
	existing["Tags"] = tags

	// Merge every external ID Stillwater has into ProviderIds, preserving any
	// IDs Jellyfin already manages (the fetched JSON deserializes inner maps
	// as map[string]any). Empty IDs are not written so a clear on the
	// Stillwater side does not silently overwrite a Jellyfin-side value;
	// explicit ID removal lives outside this push path.
	providerUpdates := buildProviderIDUpdates(data)
	if len(providerUpdates) > 0 {
		// Capture the type-assertion ok bool so a malformed fetch (Jellyfin
		// returning ProviderIds as something other than a JSON object) is
		// surfaced as an error instead of silently allocating a fresh map
		// and wiping every Jellyfin-managed ID.
		existingProviderIds := existing["ProviderIds"]
		var providerIds map[string]any
		if existingProviderIds == nil {
			providerIds = make(map[string]any, len(providerUpdates))
		} else {
			var ok bool
			providerIds, ok = existingProviderIds.(map[string]any)
			if !ok {
				return fmt.Errorf("jellyfin returned ProviderIds in an unexpected shape (%T); refusing to overwrite", existingProviderIds)
			}
		}
		for k, v := range providerUpdates {
			providerIds[k] = v
		}
		existing["ProviderIds"] = providerIds
	}

	// Map band members into Jellyfin's People array. Each entry becomes a
	// PersonInfo with Type=Person and a Role summarizing vocal type +
	// instruments. The write is gated on a non-nil BandMembers slice, which
	// only happens when the caller actually fetched members AND at least one
	// has a non-empty name; an empty Stillwater member list does NOT clear
	// the Jellyfin-side People array (same no-clobber invariant as
	// ProviderIds above -- explicit removal lives outside this push path).
	if data.BandMembers != nil {
		existing["People"] = buildPeopleEntries(data.BandMembers)
	}

	// Normalize dates to yyyy-MM-dd so Jellyfin does not silently discard.
	// Only set when normalization succeeds; an empty result would overwrite a
	// valid existing date with "" since the map-based merge has no omitempty.
	// The default branch clears the date when all source fields are empty,
	// ensuring that date clears in Stillwater propagate to Jellyfin.
	switch {
	case data.Born != "":
		normalized := connection.NormalizeDateForPlatform(data.Born)
		c.logDateNormalization("premiere_date", data.Born, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	case data.Formed != "":
		normalized := connection.NormalizeDateForPlatform(data.Formed)
		c.logDateNormalization("premiere_date", data.Formed, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	default:
		existing["PremiereDate"] = ""
	}
	switch {
	case data.Died != "":
		normalized := connection.NormalizeDateForPlatform(data.Died)
		c.logDateNormalization("end_date", data.Died, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	case data.Disbanded != "":
		normalized := connection.NormalizeDateForPlatform(data.Disbanded)
		c.logDateNormalization("end_date", data.Disbanded, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	default:
		existing["EndDate"] = ""
	}

	if err := c.postFullItem(ctx, platformArtistID, existing, "push"); err != nil {
		return err
	}

	c.Logger.Debug("metadata pushed to jellyfin", "artist_id", platformArtistID)
	return nil
}

// buildProviderIDUpdates builds the set of external-ID key/value pairs
// Stillwater wants merged into Jellyfin's ProviderIds map. Empty IDs are
// omitted so a missing-in-Stillwater value does not overwrite an
// existing-in-Jellyfin value. Key naming matches Jellyfin's metadata
// fetcher conventions: MusicBrainzArtist, TheAudioDb, Discogs, Spotify.
func buildProviderIDUpdates(data connection.ArtistPushData) map[string]string {
	ids := make(map[string]string, 4)
	if data.MusicBrainzID != "" {
		ids["MusicBrainzArtist"] = data.MusicBrainzID
	}
	if data.AudioDBID != "" {
		ids["TheAudioDb"] = data.AudioDBID
	}
	if data.DiscogsID != "" {
		ids["Discogs"] = data.DiscogsID
	}
	if data.SpotifyID != "" {
		ids["Spotify"] = data.SpotifyID
	}
	return ids
}

// buildPeopleEntries maps Stillwater's band members into Jellyfin's People
// array shape. Each entry is a map[string]any (matching how the fetched
// item body deserializes) with Name, Role, and Type=Person; Jellyfin
// treats Type=Person as a generic credited contributor, which is the
// closest existing match for a band member.
func buildPeopleEntries(members []connection.ArtistPersonRef) []map[string]any {
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		if m.Name == "" {
			continue
		}
		entry := map[string]any{
			"Name": m.Name,
			"Type": "Person",
		}
		if m.Role != "" {
			entry["Role"] = m.Role
		}
		out = append(out, entry)
	}
	return out
}

// UpdateArtistLocks persists the overall lock state for the given Jellyfin
// artist without touching content metadata. Jellyfin does NOT honor per-field
// LockedFields at the item level, so this method only syncs the whole-item
// LockData flag; the lockedFields argument is accepted for interface
// conformance and logged at debug but otherwise discarded. The item POST is a
// full replacement, so this method fetches the current item map, overwrites
// LockData, strips read-only fields, and POSTs the merged body back.
// Keeping lock sync on its own HTTP cycle (rather than piggybacking on
// PushMetadata) prevents accidental re-scrapes when LockData toggles through
// the normal metadata push path.
func (c *Client) UpdateArtistLocks(ctx context.Context, platformArtistID string, lockData bool, lockedFields []string) error {
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching artist for lock update: %w", err)
	}
	existing["LockData"] = lockData
	if len(lockedFields) > 0 {
		c.Logger.Debug("jellyfin: per-field locks ignored (not supported at item level)",
			"artist_id", platformArtistID, "field_count", len(lockedFields))
	}

	return c.postFullItem(ctx, platformArtistID, existing, "lock update")
}

// postFullItem strips read-only fields from item, marshals it, and POSTs the
// full body to /Items/{platformArtistID}. Jellyfin's POST /Items/{id} requires
// a complete item body (not a delta), so PushMetadata, UpdateArtistLocks, and
// UpdateArtistPath share this request/response cycle. The op label appears in
// error messages so callers can distinguish failures (e.g. "push failed with
// status 500", "lock update failed with status 500") without each call site
// re-implementing the request boilerplate.
//
// Thin wrapper over the shared mediabrowser.PostFullItemRaw (promoted from
// this method's former hand-rolled body, migrated onto Transport.Do). Kept as
// a method here -- rather than inlining the shared call at each call site --
// so UpdateArtistLocks (a separate, not-yet-collapsed PR) keeps compiling and
// behaving unchanged against this same private name.
func (c *Client) postFullItem(ctx context.Context, platformArtistID string, item map[string]any, op string) error {
	return mediabrowser.PostFullItemRaw(ctx, c.Client, platformArtistID, item, jellyfinReadOnlyFields, op, wrapAuthIfStatusAuth)
}

// jellyfinReadOnlyFields are item fields that Jellyfin rejects in POST
// /Items/{id}. These must be stripped from the GET response before POSTing.
var jellyfinReadOnlyFields = []string{
	"ServerId", "ImageBlurHashes", "ImageTags", "BackdropImageTags",
	"LocationType", "MediaType", "ChannelId",
}

// fetchItem retrieves a single item from Jellyfin by ID, returning the full
// item body as a generic map.
//
// Thin wrapper over the shared mediabrowser.FetchItemRaw (promoted from this
// method's former hand-rolled body, migrated onto Transport.Do). Kept as a
// method here -- rather than inlining the shared call at each call site -- so
// UpdateArtistLocks (a separate, not-yet-collapsed PR) keeps compiling and
// behaving unchanged against this same private name.
func (c *Client) fetchItem(ctx context.Context, itemID string) (map[string]any, error) {
	return mediabrowser.FetchItemRaw(ctx, c.Client, itemID, jellyfinFetchFields, wrapAuthIfStatusAuth)
}

// UploadImage uploads an image to the Jellyfin server for the given artist.
// POST /Items/{id}/Images/{type}
func (c *Client) UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error {
	jfType := mapImageType(imageType)
	if jfType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}
	return mediabrowser.UploadImageRaw(ctx, c.Client, c.Logger, "jellyfin", url.PathEscape(platformArtistID), jfType, imageType, data, contentType, wrapAuthIfStatusAuth)
}

// UploadImageAtIndex uploads an image at a specific index to the Jellyfin server.
// POST /Items/{id}/Images/{type}/{index}
// This is used for backdrop images where Jellyfin supports multiple images per artist.
func (c *Client) UploadImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int, data []byte, contentType string) error {
	jfType := mapImageType(imageType)
	return mediabrowser.UploadImageAtIndexRaw(ctx, c.Client, c.Logger, "jellyfin", url.PathEscape(platformArtistID), jfType, imageType, index, data, contentType, wrapAuthIfStatusAuth)
}

// DeleteImage deletes an image from the Jellyfin server for the given artist.
// DELETE /Items/{id}/Images/{type}
func (c *Client) DeleteImage(ctx context.Context, platformArtistID string, imageType string) error {
	jfType := mapImageType(imageType)
	return mediabrowser.DeleteImageRaw(ctx, c.Client, c.Logger, "jellyfin", url.PathEscape(platformArtistID), jfType, imageType, wrapAuthIfStatusAuth)
}

// DeleteImageAtIndex deletes the image at a specific index for the given
// artist. DELETE /Items/{id}/Images/{type}/{index}. Used to prune redundant
// backdrops on the platform (#2540 remote prune). Jellyfin re-indexes
// remaining backdrops after a delete, so callers pruning multiple indices
// MUST delete high-index-first.
func (c *Client) DeleteImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int) error {
	jfType := mapImageType(imageType)
	return mediabrowser.DeleteImageAtIndexRaw(ctx, c.Client, c.Logger, "jellyfin", url.PathEscape(platformArtistID), jfType, imageType, index, wrapAuthIfStatusAuth)
}

// logDateNormalization logs the result of normalizing a date field for push.
func (c *Client) logDateNormalization(field, raw, normalized, artistID string) {
	if normalized == "" {
		c.Logger.Warn("unparsable date dropped from push",
			"field", field, "raw", raw, "artist_id", artistID)
	} else if normalized != raw {
		c.Logger.Debug("date normalized for push",
			"field", field, "raw", raw, "normalized", normalized, "artist_id", artistID)
	}
}

// mapImageType converts a Stillwater image type to a Jellyfin image type.
func mapImageType(imageType string) string {
	switch imageType {
	case "thumb":
		return "Primary"
	case "fanart":
		return "Backdrop"
	case "logo":
		return "Logo"
	case "banner":
		return "Banner"
	default:
		return ""
	}
}
