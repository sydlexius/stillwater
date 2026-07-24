// This file collects the fetch-mutate-post HTTP mechanics shared by Emby's
// UpdateArtistLocks, UpdateArtistPath, and GetArtistPath. Emby fetches an
// artist item via the user-scoped /Users/{UserID}/Items/{id} endpoint (unlike
// Jellyfin's /Items?Ids=... query, which already goes through FetchItemRaw in
// refresh.go), so this is a separate shape rather than a reuse of
// FetchItemRaw. Only the HTTP round-trip is shared here -- the lock
// canonicalization, the field mutations, and the per-call error-message
// prefixes all stay in emby's own methods, mirroring how image_writers.go's
// doImageRequest shares the HTTP mechanics of the image write/delete methods
// while each caller keeps its own logging and semantics.
package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// FetchUserScopedItemRaw issues the GET half of Emby's user-scoped item
// fetch-mutate-post cycle: GET path (built by the caller, since the exact
// escaping of UserID/itemID is call-site specific and must not change) and
// decodes the response into a map. A JSON null response body decodes to a
// nil map; that is normalized to an empty, non-nil map here so callers can
// assign fields into it without a nil-map panic.
func FetchUserScopedItemRaw(ctx context.Context, t Transport, path string) (map[string]any, error) {
	var item map[string]any
	if err := t.Get(ctx, path, &item); err != nil {
		return nil, err
	}
	if item == nil {
		item = make(map[string]any)
	}
	return item, nil
}

// PostUserScopedItemRaw issues the POST half of the cycle: marshals item and
// POSTs it to path (built by the caller) via Transport.PostJSON. Unlike
// PostFullItemRaw this does not strip read-only fields or apply an op-labeled
// error message -- Emby's POST /Items/{id} accepts the full GET payload
// unmodified and each caller (UpdateArtistLocks, UpdateArtistPath) supplies
// its own "encoding X body" / "posting artist X update" wrap, matching what
// the hand-rolled per-method implementations did before this promotion.
func PostUserScopedItemRaw(ctx context.Context, t Transport, path string, item map[string]any) error {
	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshaling item: %w", err)
	}
	return t.PostJSON(ctx, path, bytes.NewReader(body), nil)
}
