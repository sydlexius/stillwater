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

// ErrItemNotFound marks a fetch that reached the peer, got a well-formed
// answer, and that answer was "no such item". It is the ONLY not-found signal
// callers may act on, and it exists so a caller can tell "the item is gone"
// apart from "the request failed" WITHOUT matching on error text.
//
// WHY A SENTINEL AND NOT A STRING: a caller deciding whether to DESTROY state
// (drop a stored platform link, #2426) has to distinguish absence from
// failure. Before this, both arrived as untyped fmt.Errorf values, so the only
// available discriminator was a substring match on an error message -- a
// predicate that silently becomes wrong the day a message is reworded, while
// still compiling and still passing its tests. The failure mode is deleting a
// good link because a peer returned 500. Match with errors.Is, never on text.
//
// WHAT THIS DELIBERATELY EXCLUDES is as important as what it covers:
//
//   - Any non-2xx status. A 500/502/401 says nothing about whether the item
//     exists; it says the question was not answered.
//   - A null Items[0] payload. See ErrItemAmbiguousPayload -- the peer returns
//     that shape for BOTH a tombstoned record and an access-denied one, and
//     those warrant opposite conclusions.
//   - A transport or decode failure, for the same reason as the first case.
//
// So this is a positive allow-list of ONE proven state: the peer answered 200,
// the response decoded, and the Items array was empty. Everything else stays a
// plain error, which a fail-closed caller treats as "keep what you have".
var ErrItemNotFound = errors.New("mediabrowser: item not found")

// ErrItemMalformedPayload marks a 2xx whose body decoded but carried no Items
// field at all (`{}` or `{"Items":null}`).
//
// SEPARATE from ErrItemNotFound because encoding/json decodes all three of
// `{}`, `{"Items":null}` and `{"Items":[]}` into the same nil slice, so a
// len()==0 test cannot tell "the peer said there is no such item" from "the
// peer did not answer the question". Only the third is proof of absence. The
// first two are a response that is not the shape this endpoint promises, and
// treating them as absence would let a malformed reply authorize deleting a
// valid platform link -- the exact failure this sentinel family exists to
// prevent.

// ErrItemMalformedPayload marks a 2xx whose body decoded but carried no Items
// field at all (`{}` or `{"Items":null}`).
//
// SEPARATE from ErrItemNotFound because encoding/json decodes all three of
// `{}`, `{"Items":null}` and `{"Items":[]}` into the same nil slice, so a
// len()==0 test cannot tell "the peer said there is no such item" from "the
// peer did not answer the question". Only the third is proof of absence. The
// first two are a response that is not the shape this endpoint promises, and
// treating them as absence would let a malformed reply authorize deleting a
// valid platform link -- the exact failure this sentinel family exists to
// prevent.
var ErrItemMalformedPayload = errors.New("mediabrowser: fetch response has no Items field")

// ErrItemAmbiguousPayload marks the peer returning a null Items[0]: a
// well-formed 200 whose single element is JSON null.
//
// This is SEPARATE from ErrItemNotFound on purpose, and collapsing the two
// would be a latent data-loss bug rather than a tidy-up. The peer produces
// this shape for a tombstoned record AND for one the caller may not see, and
// those demand opposite responses: the first means the link is dead, the
// second means the API key lost permission and the link is fine. Nothing in
// the response distinguishes them, so neither conclusion is available and the
// only correct answer is "I do not know".
//
// It is exported anyway rather than left an anonymous error, so a caller can
// recognize and report the ambiguity precisely ("the peer will not say")
// instead of lumping it in with transport noise. A caller must NEVER treat it
// as evidence of absence.
var ErrItemAmbiguousPayload = errors.New("mediabrowser: item returned a null payload (tombstoned or access-denied; indistinguishable)")

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

	// Items is a POINTER so a missing field and a null field stay
	// distinguishable from a present-but-empty array. encoding/json decodes
	// `{}` and `{"Items":null}` into the same nil slice as `{"Items":[]}`, so a
	// len()==0 test would classify a semantically malformed response as proof
	// of absence -- and this sentinel authorizes deleting a stored platform
	// link. An absent field means the peer did not answer the question; only an
	// explicitly empty array means it answered "no such item" (#2426 review).
	var result struct {
		Items *[]map[string]any `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding fetch response: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if result.Items == nil {
		// Missing or null Items: well-formed JSON, but it does not carry the
		// field the answer lives in. Not absence, not ambiguity about a
		// specific record -- the response simply is not the shape this endpoint
		// promises, so it proves nothing and must never license a delete.
		return nil, fmt.Errorf("item %s: fetch response has no Items field: %w",
			itemID, ErrItemMalformedPayload)
	}
	items := *result.Items
	if len(items) == 0 {
		// The one state that IS proof of absence: the peer answered, the body
		// decoded, and the Items array is PRESENT and empty. Wrapped so callers
		// match with errors.Is while the message keeps the id for logs.
		return nil, fmt.Errorf("item %s: %w", itemID, ErrItemNotFound)
	}
	// The peer can legitimately return a null Items[0] for a tombstoned or
	// access-denied record. These warrant OPPOSITE conclusions and the response
	// cannot tell them apart, so this is explicitly NOT ErrItemNotFound: a
	// caller that would destroy state on absence must treat it as unknown and
	// keep what it has. See ErrItemAmbiguousPayload.
	if items[0] == nil {
		return nil, fmt.Errorf("item %s: %w", itemID, ErrItemAmbiguousPayload)
	}
	return items[0], nil
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
