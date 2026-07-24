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
