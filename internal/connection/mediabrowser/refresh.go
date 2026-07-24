// This file collects the shared refresh/scan/write-back plumbing between
// the Emby and Jellyfin REST surfaces: TriggerLibraryScan (byte-identical),
// TriggerArtistRefresh (identical shape, platform-specific query string),
// and the fetch-item / post-full-item primitives that Jellyfin's
// UpdateArtistPath, UpdateArtistLocks, and PushMetadata all build on. Emby's
// UpdateArtistLocks and GetArtistPath are intentionally NOT touched here --
// they are a separate, behaviorally-divergent piece of work (PR 4b);
// UpdateArtistPath stays byte-identical on the Emby side too, since Emby's
// user-scoped fetch (/Users/{UserID}/Items/{id}) is structurally different
// from Jellyfin's /Items?Ids=... fetch and forcing a shared shape onto it
// would risk the EmbySilentlyDiscardsPath / NoUserID contract.
package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// TriggerLibraryScanRaw triggers a full library scan. POST /Library/Refresh
// with a nil body. Byte-identical on Emby and Jellyfin.
func TriggerLibraryScanRaw(ctx context.Context, t Transport, classifyAuth AuthErrorClassifier) error {
	if err := postNoBody(ctx, t, "/Library/Refresh"); err != nil {
		return fmt.Errorf("triggering library scan: %w", classifyAuth(err))
	}
	return nil
}

// TriggerArtistRefreshRaw forces the peer to re-import the artist's on-disk
// NFO. POST /Items/{artistID}/Refresh?{query}. Identical shape on both
// platforms, but the query string is NOT identical: Emby's includes
// ImageRefreshMode=Default, Jellyfin's omits it (Jellyfin's OpenAPI has no
// use for that param on this endpoint). Callers pass their own exact
// platform query constant (emby's reimportRefreshQuery /
// jellyfin's reimportRefreshQuery) unchanged -- this function does not
// construct or unify the query itself, so each platform's exact string is
// preserved verbatim.
func TriggerArtistRefreshRaw(ctx context.Context, t Transport, artistID, query string, classifyAuth AuthErrorClassifier) error {
	if strings.TrimSpace(artistID) == "" {
		return fmt.Errorf("artistID is required")
	}
	// PathEscape the ID so a value containing reserved characters cannot
	// break out of the URL segment; the query string carries the re-import
	// mode.
	path := fmt.Sprintf("/Items/%s/Refresh?%s", url.PathEscape(artistID), query)
	if err := postNoBody(ctx, t, path); err != nil {
		return fmt.Errorf("triggering artist refresh: %w", classifyAuth(err))
	}
	return nil
}

// postNoBody issues a POST with no body via Transport.Do and interprets the
// response the same way BaseClient.Post did (>= 300 is an error, using the
// 1 KB-bounded body reader), returning the raw *httpclient.StatusError (not
// yet classified) so callers can apply their own message prefix before
// classifying for auth.
func postNoBody(ctx context.Context, t Transport, path string) error {
	resp, err := t.Do(ctx, http.MethodPost, path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_, _ = io.Copy(io.Discard, resp.Body)
		return httpclient.NewStatusError(resp.StatusCode, string(buf))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// FetchItemRaw retrieves a single item by ID as a generic map, via
// GET /Items?Ids={id}&Fields={fields}. This is Jellyfin's private fetchItem
// promoted to a shared free function and migrated onto Transport.Do (it
// previously hand-rolled http.NewRequestWithContext + c.HTTPClient.Do).
// Emby does not use this: its UpdateArtistLocks/UpdateArtistPath/GetArtistPath
// fetch via the user-scoped /Users/{UserID}/Items/{id} endpoint instead,
// which needs no Fields query and no Ids-based not-found handling, so it
// stays Emby-side.
//
// fields is the exact Fields query value the caller wants (Jellyfin's
// UpdateArtistLocks/UpdateArtistPath/PushMetadata all go through the
// jellyfinFetchFields constant on the Jellyfin side, unchanged from before
// this promotion). An empty/whitespace-only itemID is rejected before any
// request is sent: building "Ids=" would return the library's first item,
// silently corrupting whichever write follows.
func FetchItemRaw(ctx context.Context, t Transport, itemID, fields string, classifyAuth AuthErrorClassifier) (map[string]any, error) {
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("item id is required")
	}
	path := fmt.Sprintf("/Items?Ids=%s&Fields=%s", url.QueryEscape(itemID), fields)
	resp, err := t.Do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, fmt.Errorf("executing fetch request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := httpclient.ReadBoundedStatusError(resp)
		formatted := fmt.Errorf("fetch failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return nil, classifyAuth(errors.Join(formatted, statusErr))
	}

	var result struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding fetch response: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if len(result.Items) == 0 {
		return nil, fmt.Errorf("item %s not found", itemID)
	}
	// The peer can legitimately return a null Items[0] for a tombstoned or
	// access-denied record. Treat it as "not found" rather than returning a
	// nil map that would panic on the caller's first write.
	if result.Items[0] == nil {
		return nil, fmt.Errorf("item %s returned null payload", itemID)
	}
	return result.Items[0], nil
}

// PostFullItemRaw strips readOnlyFields from item, marshals it, and POSTs
// the full body to /Items/{itemID}. This is Jellyfin's private postFullItem
// promoted to a shared free function and migrated onto Transport.Do (it
// previously hand-rolled http.NewRequestWithContext + c.HTTPClient.Do). Both
// PushMetadata and UpdateArtistLocks share this request/response cycle on
// Jellyfin; UpdateArtistPath (this PR) also uses it. op appears in error
// messages ("push failed with status 500", "lock update failed with status
// 500", "path update failed with status 500") so callers can distinguish
// failures without each call site re-implementing the request boilerplate.
//
// readOnlyFields is a parameter (not a package-level list) because the set
// of fields a peer rejects on POST is platform-specific; Jellyfin passes its
// existing jellyfinReadOnlyFields unchanged.
func PostFullItemRaw(ctx context.Context, t Transport, itemID string, item map[string]any, readOnlyFields []string, op string, classifyAuth AuthErrorClassifier) error {
	// Operate on a shallow copy so callers that retain `item` after this
	// call (for example to log it on error or pass it to a retry) see
	// their original map unchanged.
	cleanItem := make(map[string]any, len(item))
	for k, v := range item {
		cleanItem[k] = v
	}
	for _, key := range readOnlyFields {
		delete(cleanItem, key)
	}

	payload, err := json.Marshal(cleanItem)
	if err != nil {
		return fmt.Errorf("marshaling %s body: %w", op, err)
	}

	path := fmt.Sprintf("/Items/%s", url.PathEscape(itemID))
	resp, err := t.Do(ctx, http.MethodPost, path, bytes.NewReader(payload), "application/json")
	if err != nil {
		return fmt.Errorf("executing %s request: %w", op, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := httpclient.ReadBoundedStatusError(resp)
		formatted := fmt.Errorf("%s failed with status %d: %s", op, statusErr.StatusCode, statusErr.Body)
		return classifyAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
