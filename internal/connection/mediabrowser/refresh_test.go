package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// embyStyleRefreshQuery and jellyfinStyleRefreshQuery mirror the exact
// per-platform constants TriggerArtistRefreshRaw's real callers pass
// (emby.reimportRefreshQuery / jellyfin.reimportRefreshQuery as of this
// PR): Emby's includes ImageRefreshMode=Default, Jellyfin's omits it. This
// file asserts TriggerArtistRefreshRaw reproduces each caller's query
// verbatim rather than unifying them -- the collapse's most important
// invariant.
const (
	embyStyleRefreshQuery     = "MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ImageRefreshMode=Default&ReplaceAllImages=false"
	jellyfinStyleRefreshQuery = "MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ReplaceAllImages=false"
)

func TestTriggerLibraryScanRaw_IssuesPost(t *testing.T) {
	tr := &rawTransport{}
	if err := TriggerLibraryScanRaw(context.Background(), tr, noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", tr.gotDoMethod)
	}
	if tr.gotDoPath != "/Library/Refresh" {
		t.Errorf("path = %q, want /Library/Refresh", tr.gotDoPath)
	}
	if tr.gotDoBody != "" {
		t.Errorf("body = %q, want empty (nil body)", tr.gotDoBody)
	}
}

func TestTriggerLibraryScanRaw_ServerErrorRoutesThroughClassifier(t *testing.T) {
	wantSentinel := errors.New("sentinel")
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.Join(err, wantSentinel)
	}
	tr := &rawTransport{doStatus: 500, doBody: "boom"}
	err := TriggerLibraryScanRaw(context.Background(), tr, classify)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !errors.Is(err, wantSentinel) {
		t.Error("expected classifier's sentinel to be reachable via errors.Is")
	}
}

func TestTriggerLibraryScanRaw_DoErrorPropagates(t *testing.T) {
	wantErr := errors.New("network boom")
	tr := &rawTransport{doErr: wantErr}
	err := TriggerLibraryScanRaw(context.Background(), tr, noopClassifier)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped Do error, got %v", err)
	}
}

// TestTriggerArtistRefreshRaw_PreservesEmbyQuery is the revert-and-rerun
// canary for the query-preservation contract: it asserts the exact query
// string an Emby-shaped caller passes reaches the wire unmodified,
// including the ImageRefreshMode=Default param that Jellyfin's own query
// omits. A collapse that unified the two platforms' queries would flip
// this test RED.
func TestTriggerArtistRefreshRaw_PreservesEmbyQuery(t *testing.T) {
	tr := &rawTransport{}
	if err := TriggerArtistRefreshRaw(context.Background(), tr, "emby-001", embyStyleRefreshQuery, noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/Items/emby-001/Refresh?MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ImageRefreshMode=Default&ReplaceAllImages=false"
	if tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
	if got := extractQueryValue(tr.gotDoPath, "ImageRefreshMode"); got != "Default" {
		t.Errorf("ImageRefreshMode = %q, want Default", got)
	}
}

// TestTriggerArtistRefreshRaw_PreservesJellyfinQuery mirrors the Emby test
// for Jellyfin's shape: Jellyfin's reimportRefreshQuery has NO
// ImageRefreshMode param at all. If the shared function ever injected one
// (accidentally unifying the two platforms), this test catches it.
func TestTriggerArtistRefreshRaw_PreservesJellyfinQuery(t *testing.T) {
	tr := &rawTransport{}
	if err := TriggerArtistRefreshRaw(context.Background(), tr, "jf-001", jellyfinStyleRefreshQuery, noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/Items/jf-001/Refresh?MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ReplaceAllImages=false"
	if tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
	if got := extractQueryValue(tr.gotDoPath, "ImageRefreshMode"); got != "" {
		t.Errorf("ImageRefreshMode = %q, want absent for the Jellyfin-shaped query", got)
	}
}

func TestTriggerArtistRefreshRaw_EmptyArtistID(t *testing.T) {
	tr := &rawTransport{}
	if err := TriggerArtistRefreshRaw(context.Background(), tr, "   ", embyStyleRefreshQuery, noopClassifier); err == nil {
		t.Fatal("expected error for blank artistID")
	}
	if tr.gotDoMethod != "" {
		t.Error("blank artistID must not issue a request")
	}
}

// extractQueryValue is a tiny helper so the tests above assert against a
// parsed query param rather than a raw substring match, matching the
// rigor of the pre-existing emby/jellyfin client_test.go query assertions.
func extractQueryValue(pathWithQuery, key string) string {
	i := strings.IndexByte(pathWithQuery, '?')
	if i < 0 {
		return ""
	}
	values, err := url.ParseQuery(pathWithQuery[i+1:])
	if err != nil {
		return ""
	}
	return values.Get(key)
}

// TestFetchItemRaw_IssuesGetWithFields is the revert-and-rerun canary for
// the Fields query-list preservation: it asserts the exact Fields value
// reaches the wire, matching Jellyfin's pre-promotion fetchItem behavior.
func TestFetchItemRaw_IssuesGetWithFields(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items":[{"Id":"jf-a1","Name":"Test"}]}`}
	item, err := FetchItemRaw(context.Background(), tr, "jf-a1", "Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,LockData,LockedFields", noopClassifier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/Items?Ids=jf-a1&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,LockData,LockedFields"
	if tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
	if tr.gotDoMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", tr.gotDoMethod)
	}
	if item["Id"] != "jf-a1" {
		t.Errorf("decoded item = %+v, want Id=jf-a1", item)
	}
}

func TestFetchItemRaw_EmptyItemID(t *testing.T) {
	tr := &rawTransport{}
	_, err := FetchItemRaw(context.Background(), tr, "   ", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected error for blank itemID")
	}
	if tr.gotDoMethod != "" {
		t.Error("blank itemID must not issue a request")
	}
}

func TestFetchItemRaw_NotFound(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items":[]}`}
	_, err := FetchItemRaw(context.Background(), tr, "missing", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected not-found error for empty Items")
	}
}

func TestFetchItemRaw_NullFirstItem(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items":[null]}`}
	_, err := FetchItemRaw(context.Background(), tr, "tombstoned", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected error for a null first item")
	}
}

func TestFetchItemRaw_ServerErrorRoutesThroughClassifier(t *testing.T) {
	wantSentinel := errors.New("sentinel")
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.Join(err, wantSentinel)
	}
	tr := &rawTransport{doStatus: 500, doBody: "boom"}
	_, err := FetchItemRaw(context.Background(), tr, "a1", "Overview", classify)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !errors.Is(err, wantSentinel) {
		t.Error("expected classifier's sentinel to be reachable via errors.Is")
	}
}

func TestFetchItemRaw_DoErrorPropagates(t *testing.T) {
	wantErr := errors.New("network boom")
	tr := &rawTransport{doErr: wantErr}
	_, err := FetchItemRaw(context.Background(), tr, "a1", "Overview", noopClassifier)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped Do error, got %v", err)
	}
}

func TestPostFullItemRaw_StripsReadOnlyFieldsAndPosts(t *testing.T) {
	tr := &rawTransport{}
	item := map[string]any{
		"Id":           "a1",
		"Path":         "/new/path",
		"ServerId":     "should-be-stripped",
		"ImageTags":    map[string]any{"Primary": "abc"},
		"LocationType": "FileSystem",
	}
	if err := PostFullItemRaw(context.Background(), tr, "a1", item, jellyfinReadOnlyFieldsForTest, "path update", noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", tr.gotDoMethod)
	}
	if want := "/Items/a1"; tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
	if tr.gotDoContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", tr.gotDoContentType)
	}
	var posted map[string]any
	if err := json.Unmarshal([]byte(tr.gotDoBody), &posted); err != nil {
		t.Fatalf("posted body did not decode as JSON: %v", err)
	}
	for _, stripped := range jellyfinReadOnlyFieldsForTest {
		if _, ok := posted[stripped]; ok {
			t.Errorf("posted body still contains read-only field %q: %+v", stripped, posted)
		}
	}
	if posted["Path"] != "/new/path" {
		t.Errorf("Path = %v, want /new/path", posted["Path"])
	}
	// The original map passed in must be untouched (shallow-copy contract).
	if _, ok := item["ServerId"]; !ok {
		t.Error("caller's original item map was mutated; PostFullItemRaw must operate on a copy")
	}
}

func TestPostFullItemRaw_ServerErrorRoutesThroughClassifier(t *testing.T) {
	wantSentinel := errors.New("sentinel")
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.Join(err, wantSentinel)
	}
	tr := &rawTransport{doStatus: 500, doBody: "boom"}
	err := PostFullItemRaw(context.Background(), tr, "a1", map[string]any{"Id": "a1"}, nil, "lock update", classify)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !errors.Is(err, wantSentinel) {
		t.Error("expected classifier's sentinel to be reachable via errors.Is")
	}
}

func TestPostFullItemRaw_DoErrorPropagates(t *testing.T) {
	wantErr := errors.New("network boom")
	tr := &rawTransport{doErr: wantErr}
	err := PostFullItemRaw(context.Background(), tr, "a1", map[string]any{"Id": "a1"}, nil, "push", noopClassifier)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped Do error, got %v", err)
	}
}

// jellyfinReadOnlyFieldsForTest mirrors jellyfin.jellyfinReadOnlyFields
// without importing the jellyfin package (which would create an import
// cycle back into mediabrowser).
var jellyfinReadOnlyFieldsForTest = []string{
	"ServerId", "ImageBlurHashes", "ImageTags", "BackdropImageTags",
	"LocationType", "MediaType", "ChannelId",
}

// --- #2426 evidence primitives: not-found vs. everything else ---

// TestFetchItemRaw_EmptyItemsIsErrItemNotFound pins the ONE state that counts
// as proof of absence: the peer answered 200, the body decoded, and the Items
// array is empty.
//
// Asserted via errors.Is, never a substring match on the message -- the whole
// point of the sentinel is that a caller deciding whether to DESTROY state
// (drop a platform link, #2426) must not depend on error wording that a
// reword would silently break while still compiling.
func TestFetchItemRaw_EmptyItemsIsErrItemNotFound(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items":[]}`}

	_, err := FetchItemRaw(context.Background(), tr, "jf-gone", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected an error for an empty Items array")
	}
	if !errors.Is(err, ErrItemNotFound) {
		t.Errorf("error %v is not ErrItemNotFound; a caller cannot recognize a genuine absence", err)
	}
	if !strings.Contains(err.Error(), "jf-gone") {
		t.Errorf("error %q drops the item id, making the log unactionable", err.Error())
	}
}

// TestFetchItemRaw_NullPayloadIsNotNotFound is the most important test in this
// file. The peer returns a null Items[0] for a TOMBSTONED record AND for an
// ACCESS-DENIED one; those demand opposite responses (the link is dead vs. the
// API key lost permission and the link is fine), and the response cannot tell
// them apart.
//
// So this asserts the ambiguity is preserved rather than resolved: it must NOT
// satisfy errors.Is(ErrItemNotFound). Folding these together would look like a
// tidy-up and would license deleting a good link the first time a permission
// changed.
func TestFetchItemRaw_NullPayloadIsNotNotFound(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items":[null]}`}

	_, err := FetchItemRaw(context.Background(), tr, "jf-ambiguous", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected an error for a null Items[0]")
	}
	if errors.Is(err, ErrItemNotFound) {
		t.Error("a null payload reported as ErrItemNotFound: tombstoned and access-denied " +
			"are indistinguishable here, so neither conclusion is available")
	}
	if !errors.Is(err, ErrItemAmbiguousPayload) {
		t.Errorf("error %v is not ErrItemAmbiguousPayload; the caller cannot report the "+
			"ambiguity precisely and will lump it in with transport noise", err)
	}
}

// TestFetchItemRaw_ServerErrorIsNotNotFound covers the failure that would be
// most catastrophic to misread. A 500 says the question was not answered; it
// says nothing about whether the item exists. If this ever satisfied
// ErrItemNotFound, every artist's link would be a candidate for deletion the
// next time a peer had a bad minute.
func TestFetchItemRaw_ServerErrorIsNotNotFound(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			tr := &rawTransport{doStatus: status, doBody: `{"error":"nope"}`}

			_, err := FetchItemRaw(context.Background(), tr, "jf-a1", "Overview", noopClassifier)
			if err == nil {
				t.Fatalf("expected an error for status %d", status)
			}
			if errors.Is(err, ErrItemNotFound) {
				t.Errorf("status %d reported as ErrItemNotFound; a failed request is not "+
					"evidence the item is gone", status)
			}
		})
	}
}

// TestFetchItemRaw_TransportErrorIsNotNotFound is the unreachable-peer case:
// same rule, different layer.
func TestFetchItemRaw_TransportErrorIsNotNotFound(t *testing.T) {
	tr := &rawTransport{doErr: errors.New("dial tcp: connection refused")}

	_, err := FetchItemRaw(context.Background(), tr, "jf-a1", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected an error from an unreachable peer")
	}
	if errors.Is(err, ErrItemNotFound) {
		t.Error("an unreachable peer reported as ErrItemNotFound")
	}
}

// TestFetchItemRaw_MalformedBodyIsNotNotFound: a body that does not decode is
// an unanswered question too. Notably a 200 with garbage must not fall through
// to the empty-Items branch.
func TestFetchItemRaw_MalformedBodyIsNotNotFound(t *testing.T) {
	tr := &rawTransport{doBody: `{"Items": not json`}

	_, err := FetchItemRaw(context.Background(), tr, "jf-a1", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if errors.Is(err, ErrItemNotFound) {
		t.Error("a malformed body reported as ErrItemNotFound")
	}
}

// TestFetchItemRaw_BlankItemIDIsNotNotFound: the caller's own bad input is a
// programming error, not a statement about the peer's contents.
func TestFetchItemRaw_BlankItemIDIsNotNotFound(t *testing.T) {
	tr := &rawTransport{}

	_, err := FetchItemRaw(context.Background(), tr, "  ", "Overview", noopClassifier)
	if err == nil {
		t.Fatal("expected an error for a blank item id")
	}
	if errors.Is(err, ErrItemNotFound) {
		t.Error("a blank item id reported as ErrItemNotFound")
	}
}
