package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// TestHandlePullMetadata_MatchingFieldNotInUpdated verifies that when the
// platform value already matches the stored value, that field is NOT included
// in the "updated" list (the changed bool is false, so the append is skipped).
//
// The test stands up a minimal httptest.Server that mimics just enough of the
// Emby API for GetArtistDetail to succeed, then calls handlePullMetadata and
// checks the JSON response.
func TestHandlePullMetadata_MatchingFieldNotInUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()

	// Create an artist with a known biography.
	a := addTestArtist(t, artistSvc, "Pull Noop Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "existing bio"); err != nil {
		t.Fatalf("setting initial biography: %v", err)
	}

	// Stand up a stub Emby server that returns the SAME biography the artist
	// already has. The handler should not include "biography" in updated.
	embyResp := `{"Name":"Pull Noop Artist","SortName":"Pull Noop Artist","Overview":"existing bio","Genres":[],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockData":false,"LockedFields":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, embyResp)
	}))
	defer srv.Close()

	// Create a connection pointing to the test server.
	conn := &connection.Connection{
		Name:    "Test Emby",
		Type:    connection.TypeEmby,
		URL:     srv.URL,
		APIKey:  "test-api-key",
		Emby:    &connection.EmbyConfig{PlatformUserID: "user-001"},
		Enabled: true,
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	// Associate a platform artist ID with this artist on this connection.
	platformID := "emby-artist-001"
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, platformID); err != nil {
		t.Fatalf("setting platform artist ID: %v", err)
	}

	// Call handlePullMetadata.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// When updated is nil (no fields changed), the JSON will encode as null.
	// Either null or an empty slice satisfies "biography not in updated".
	if updatedRaw := resp["updated"]; updatedRaw != nil {
		updated, ok := updatedRaw.([]any)
		if !ok {
			t.Fatalf("updated is not an array: %T %v", updatedRaw, updatedRaw)
		}
		for _, v := range updated {
			if v == "biography" {
				t.Errorf("biography appears in updated=%v but the value already matched the platform; want it absent", updated)
			}
		}
	}
}

// TestHandlePullMetadata_ChangedFieldInUpdated verifies that when the platform
// returns a value that differs from the stored value, that field IS included in
// the "updated" list (changed==true).
func TestHandlePullMetadata_ChangedFieldInUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()

	a := addTestArtist(t, artistSvc, "Pull Changed Artist")
	// Artist starts with no biography / no genres; platform returns new values for
	// both plus dates so all 4 UpdateField branches are exercised.
	embyResp := `{"Name":"Pull Changed Artist","SortName":"Pull Changed Artist","Overview":"new platform bio","Genres":["Rock","Grunge"],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockData":false,"LockedFields":[],"PremiereDate":"1990-01-01T00:00:00.0000000Z","EndDate":"2000-06-01T00:00:00.0000000Z"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, embyResp)
	}))
	defer srv.Close()

	conn := &connection.Connection{
		Name:    "Test Emby Changed",
		Type:    connection.TypeEmby,
		URL:     srv.URL,
		APIKey:  "test-api-key",
		Emby:    &connection.EmbyConfig{PlatformUserID: "user-001"},
		Enabled: true,
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-artist-002"); err != nil {
		t.Fatalf("setting platform artist ID: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	updatedRaw := resp["updated"]
	updated, ok := updatedRaw.([]any)
	if !ok || len(updated) == 0 {
		t.Fatalf("updated = %v, want [biography] (new platform value differs from stored value)", updatedRaw)
	}
	found := false
	for _, v := range updated {
		if v == "biography" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("biography not in updated=%v, want it present (field actually changed)", updated)
	}
}

// pullGenres seeds an artist with stored genres, serves the given platform
// genres from a stub Emby, runs the pull, and returns the artist's resulting
// genres plus the response's updated list.
func pullGenres(t *testing.T, r *Router, artistSvc *artist.Service, stored, platform []string, lang ...string) ([]string, []any) {
	t.Helper()
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Pull Genres Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "genres", strings.Join(stored, ", ")); err != nil {
		t.Fatalf("seeding genres: %v", err)
	}
	quoted, _ := json.Marshal(platform)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Name":"x","Genres":%s,"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockedFields":[]}`, quoted)
	}))
	t.Cleanup(srv.Close)
	conn := &connection.Connection{Name: "Emby", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
		Emby: &connection.EmbyConfig{PlatformUserID: "u"}, Enabled: true}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-1"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	if len(lang) > 0 {
		// Real preference path: the handler reads metadata_languages for the
		// authenticated user from user_preferences.
		seedUserPref(t, r, "test-user", PrefMetadataLanguages, `["`+lang[0]+`"]`)
		req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	}
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Updated []any `json:"updated"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	got, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	return got.Genres, resp.Updated
}

func TestHandlePullMetadata_GenresAccumulate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		stored, pull []string
		lang         []string
		want         []string
		wantUpdated  bool
	}{
		{"adds platform genre", []string{"Rock", "Blues"}, []string{"Folk"}, nil, []string{"Rock", "Blues", "Folk"}, true},
		{"canonical dedup", []string{"Rock"}, []string{"rock", "Jazz"}, nil, []string{"Rock", "Jazz"}, true},
		{"subset is a no-op", []string{"Rock", "Jazz"}, []string{"Rock"}, nil, []string{"Rock", "Jazz"}, false},
		// Canonicalization side effect: stored tags are canonicalized too.
		{"canonicalizes stored tags", []string{"rock", "hip hop"}, []string{"Rock"}, nil, []string{"Rock", "Hip-Hop"}, true},
		// Japanese preference: stored Rock and pulled ロック collapse to one
		// localized entry instead of two.
		{"locale-aware dedup", []string{"Rock"}, []string{"ロック"}, []string{"ja"}, []string{"ロック"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, artistSvc, _ := testRouterWithHistory(t)
			got, updated := pullGenres(t, r, artistSvc, tt.stored, tt.pull, tt.lang...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("genres = %v, want %v", got, tt.want)
			}
			if has := slices.Contains(updated, any("genres")); has != tt.wantUpdated {
				t.Errorf("genres in updated=%v is %v, want %v", updated, has, tt.wantUpdated)
			}
		})
	}
}

// TestHandlePullMetadata_GenresApplyExcludePatterns saves an exclude pattern
// through the real settings handler and checks it strips a stored tag from the
// pulled union, not just from the incoming platform list.
func TestHandlePullMetadata_GenresApplyExcludePatterns(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	put := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(`{"exclude":["blues"]}`))
	pw := httptest.NewRecorder()
	r.handlePutVocab(pw, put)
	if pw.Code != http.StatusOK {
		t.Fatalf("saving vocab: %d %s", pw.Code, pw.Body.String())
	}
	got, _ := pullGenres(t, r, artistSvc, []string{"Rock", "Blues"}, []string{"Folk"})
	if want := []string{"Rock", "Folk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("genres = %v, want %v", got, want)
	}
}

// TestHandleFieldUpdate_GenresStillReplace pins the precondition that the
// shared UpdateField verb replaces: only the pull handler accumulates.
func TestHandleFieldUpdate_GenresStillReplace(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Manual Genres Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "genres", "Rock, Blues"); err != nil {
		t.Fatalf("seeding genres: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/genres", strings.NewReader("value=Folk"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "genres")
	w := httptest.NewRecorder()
	r.handleFieldUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	got, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if want := []string{"Folk"}; !reflect.DeepEqual(got.Genres, want) {
		t.Errorf("genres = %v, want %v", got.Genres, want)
	}
}

// Two concurrent pulls on one artist must keep both platforms' genres; the
// read hook widens the window so a missing lock loses one deterministically.
func TestHandlePullMetadata_ConcurrentPullsKeepBothGenres(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()
	r.pullGenresReadHook = func() { time.Sleep(150 * time.Millisecond) }
	a := addTestArtist(t, artistSvc, "Concurrent Pull Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "genres", "Rock"); err != nil {
		t.Fatalf("seeding genres: %v", err)
	}
	var reqs []*http.Request
	for i, g := range []string{"Folk", "Jazz"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Name":"x","Genres":[%q],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockedFields":[]}`, g)
		}))
		t.Cleanup(srv.Close)
		conn := &connection.Connection{Name: fmt.Sprintf("Emby %d", i), Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
			Emby: &connection.EmbyConfig{PlatformUserID: "u"}, Enabled: true}
		if err := r.connectionService.Create(ctx, conn); err != nil {
			t.Fatalf("creating connection: %v", err)
		}
		if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, fmt.Sprintf("emby-%d", i)); err != nil {
			t.Fatalf("setting platform id: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
		req.SetPathValue("id", a.ID)
		reqs = append(reqs, req)
	}
	var wg sync.WaitGroup
	for _, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.handlePullMetadata(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
	got, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	for _, want := range []string{"Rock", "Folk", "Jazz"} {
		if !slices.Contains(got.Genres, want) {
			t.Errorf("genres = %v, missing %q (lost update)", got.Genres, want)
		}
	}
}

// TestHandlePullMetadata_FailedReReadSkipsGenres makes the locked re-read
// fail by deleting the artist while the platform call is in flight (after the
// handler's first load). The pull must skip genres rather than write from the
// stale snapshot: on a deleted row that write would "succeed" as a no-op and
// report "genres" as updated.
func TestHandlePullMetadata_FailedReReadSkipsGenres(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Vanishing Artist")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := artistSvc.Delete(ctx, a.ID); err != nil {
			t.Errorf("deleting artist mid-pull: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Name":"x","Genres":["Folk"],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockedFields":[]}`)
	}))
	t.Cleanup(srv.Close)
	conn := &connection.Connection{Name: "Emby", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
		Emby: &connection.EmbyConfig{PlatformUserID: "u"}, Enabled: true}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-1"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Updated []any `json:"updated"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if slices.Contains(resp.Updated, any("genres")) {
		t.Errorf("updated = %v, want genres absent after a failed re-read", resp.Updated)
	}
	if _, err := artistSvc.GetByID(ctx, a.ID); err == nil {
		t.Error("artist still exists; the test did not make the re-read fail")
	}
}

// TestNewStateGetter_AllowListMatchesSupportsPlatformState verifies that the
// newStateGetter handler switch (Emby, Jellyfin) agrees with
// connection.SupportsPlatformState on which types are supported.
func TestNewStateGetter_AllowListMatchesSupportsPlatformState(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	types := []string{
		connection.TypeEmby,
		connection.TypeJellyfin,
		connection.TypeLidarr,
	}

	for _, typ := range types {
		typ := typ
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			supports := connection.SupportsPlatformState(typ)
			conn := &connection.Connection{Type: typ, URL: "http://x", APIKey: "k"}
			_, err := r.newStateGetter(conn)
			getterSupports := !errors.Is(err, errUnsupportedConnectionType)

			if supports != getterSupports {
				t.Errorf("type=%s: SupportsPlatformState=%v, but newStateGetter error=%v (want agreement)", typ, supports, err)
			}
		})
	}
}

// TestHandlePullMetadata_DateFieldsFollowNormalizedType pins that the pulled
// platform dates land in born/died for a "person" regardless of the stored
// type's case or padding (#3333), and in formed/disbanded for any other type.
func TestHandlePullMetadata_DateFieldsFollowNormalizedType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, typ  string
		wantPerson bool
	}{
		{"canonical person", "person", true},
		{"mixed case padded person", " Person ", true},
		{"group", "group", false},
		// Pinned, not endorsed: pull treats only "person" as born/died, while push
		// treats "solo" as born/died. This is the existing vocabulary mismatch.
		{"solo", "solo", false},
		{"unknown type", "banana", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, artistSvc, _ := testRouterWithHistory(t)
			ctx := context.Background()
			a := &artist.Artist{Name: "Pull Type " + tc.name, SortName: "x", Type: tc.typ, Path: "/music/pt-" + tc.name}
			if err := artistSvc.Create(ctx, a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"Name":"x","Genres":[],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockedFields":[],"PremiereDate":"1990-01-01T00:00:00.0000000Z","EndDate":"2000-06-01T00:00:00.0000000Z"}`)
			}))
			t.Cleanup(srv.Close)
			conn := &connection.Connection{Name: "Emby", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
				Emby: &connection.EmbyConfig{PlatformUserID: "u"}, Enabled: true}
			if err := r.connectionService.Create(ctx, conn); err != nil {
				t.Fatalf("creating connection: %v", err)
			}
			if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-1"); err != nil {
				t.Fatalf("setting platform id: %v", err)
			}
			// Precondition: the stored type is exactly what the case supplied.
			if pre, err := artistSvc.GetByID(ctx, a.ID); err != nil || pre.Type != tc.typ {
				t.Fatalf("precondition: stored type = %q (err %v), want %q", pre.Type, err, tc.typ)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
			req.SetPathValue("id", a.ID)
			w := httptest.NewRecorder()
			r.handlePullMetadata(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
			}
			got, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading artist: %v", err)
			}
			if tc.wantPerson {
				if got.Born == "" || got.Died == "" || got.Formed != "" || got.Disbanded != "" {
					t.Errorf("person: born=%q died=%q formed=%q disbanded=%q, want only born/died set", got.Born, got.Died, got.Formed, got.Disbanded)
				}
			} else if got.Formed == "" || got.Disbanded == "" || got.Born != "" || got.Died != "" {
				t.Errorf("non-person: born=%q died=%q formed=%q disbanded=%q, want only formed/disbanded set", got.Born, got.Died, got.Formed, got.Disbanded)
			}
		})
	}
}
