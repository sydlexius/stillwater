package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
)

// fakePlatform models the platform: slots hold bytes keyed "artist/type/slot"
// (absent = 404); every fetch is recorded so idempotence is observable.
type fakePlatform struct {
	slots   map[string][]byte
	calls   []string
	onFetch func(key string) // runs inside a fetch, i.e. after the pass drained its rows
}

func platKey(artistID, imageType string) string { return artistID + "/" + imageType + "/0" }

func (f *fakePlatform) FetchArtistImageSlot(_ context.Context, artistID, imageType string, slot int) ([]byte, string, error) {
	k := fmt.Sprintf("%s/%s/%d", artistID, imageType, slot)
	f.calls = append(f.calls, k)
	if f.onFetch != nil {
		f.onFetch(k)
	}
	b, ok := f.slots[k]
	if !ok {
		return nil, "", errors.New("404 not found")
	}
	return b, "image/jpeg", nil
}

// seed inserts an artist on an enabled connectionType with one unhashed row per type, and a distinct platform picture each.
func seed(t *testing.T, db *sql.DB, f *fakePlatform, id, path, connType string, types ...string) {
	t.Helper()
	exec(t, db, fmt.Sprintf(`INSERT INTO artists (id, name, path) VALUES ('%[1]s', '%[1]s', '%s')`, id, path))
	exec(t, db, fmt.Sprintf(`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled) VALUES ('conn-%s', 'c', '%s', 'http://x', 'k', 1)`, id, connType))
	exec(t, db, fmt.Sprintf(`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id) VALUES ('%[1]s', 'conn-%[1]s', 'plat-%[1]s')`, id))
	for _, typ := range types {
		exec(t, db, fmt.Sprintf(`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			VALUES ('%[1]s-%[2]s', '%[1]s', '%[2]s', 0, 1, '', '')`, id, typ))
		if _, ok := f.slots[platKey(id, typ)]; !ok {
			f.slots[platKey(id, typ)] = backfillJPEG(t, len(f.slots)+1)
		}
	}
}

func hashesOf(t *testing.T, db *sql.DB, id, typ string) (p, c string) {
	t.Helper()
	if err := db.QueryRow(`SELECT phash, content_hash FROM artist_images WHERE artist_id=? AND image_type=?`, id, typ).Scan(&p, &c); err != nil {
		t.Fatalf("reading hashes: %v", err)
	}
	return p, c
}

func exec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func newPass(t *testing.T) (*Service, *sql.DB, string, *fakePlatform) {
	t.Helper()
	db, dbPath := setupTestDBWithImages(t)
	cache := t.TempDir()
	return NewService(db, dbPath, cache, slog.Default()), db, cache, &fakePlatform{slots: map[string][]byte{}}
}

var fastOpts = PlatformHashBackfillOpts{RequestsPerMinute: 60000}

// Each single-image row gets its own hashes; a re-run fetches nothing; fanart (#3602) is untouched.
func TestBackfillPlatformImageHashes_FillsAndIsIdempotent(t *testing.T) {
	svc, db, _, fake := newPass(t)
	seed(t, db, fake, "a", "", "emby", "thumb", "logo", "banner", "fanart")
	res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, fastOpts)
	if err != nil || res.Filled != 3 || res.Failed != 0 {
		t.Fatalf("result = %+v, %v; want 3 filled", res, err)
	}
	for _, typ := range []string{"thumb", "logo", "banner"} {
		b := fake.slots[platKey("a", typ)]
		ph, _ := img.PerceptualHash(bytes.NewReader(b))
		if p, c := hashesOf(t, db, "a", typ); p != img.HashHex(ph) || c != img.ContentHash(b) {
			t.Errorf("%s hashes = %q/%q, want its own bytes' hashes", typ, p, c)
		}
	}
	if p, c := hashesOf(t, db, "a", "fanart"); p != "" || c != "" {
		t.Errorf("fanart row written %q/%q", p, c)
	}
	if len(fake.calls) != 3 {
		t.Errorf("fetched %v, want exactly the 3 single-image rows", fake.calls)
	}
	fake.calls = nil
	if res, err = svc.BackfillPlatformImageHashes(context.Background(), fake, fastOpts); err != nil || len(fake.calls) != 0 || res.Selected != 0 {
		t.Errorf("re-run fetched %v result %+v, %v; want nothing", fake.calls, res, err)
	}
}

// A 404, an empty body and corrupt bytes leave rows unhashed; the healthy sibling is filled.
func TestBackfillPlatformImageHashes_FailuresLeaveRowsUnhashed(t *testing.T) {
	svc, db, _, fake := newPass(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		seed(t, db, fake, id, "", "jellyfin", "thumb")
	}
	fake.slots[platKey("b", "thumb")] = []byte("not an image")
	fake.slots[platKey("c", "thumb")] = nil
	delete(fake.slots, platKey("d", "thumb"))
	res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, fastOpts)
	if err != nil || res.Filled != 1 || res.Failed != 3 {
		t.Fatalf("result = %+v, %v; want 1 filled 3 failed", res, err)
	}
	for _, id := range []string{"b", "c", "d"} {
		if p, c := hashesOf(t, db, id, "thumb"); p != "" || c != "" {
			t.Errorf("%s hashes = %q/%q, a failure must never be written", id, p, c)
		}
	}
}

// Path-backed, disabled-connection and Lidarr-only artists are skipped; an existing phash survives.
func TestBackfillPlatformImageHashes_SelectionAndPreservation(t *testing.T) {
	svc, db, _, fake := newPass(t)
	seed(t, db, fake, "local", t.TempDir(), "emby", "thumb")
	seed(t, db, fake, "lid", "", "lidarr", "thumb")
	seed(t, db, fake, "off", "", "emby", "thumb")
	seed(t, db, fake, "keep", "", "emby", "thumb", "logo", "banner")
	exec(t, db, `UPDATE connections SET enabled = 0 WHERE id = 'conn-off'`)
	exec(t, db, `UPDATE artist_images SET exists_flag = 0 WHERE artist_id='keep' AND image_type IN ('logo', 'banner')`)
	exec(t, db, `UPDATE artist_images SET phash='keepme0000000000' WHERE artist_id='keep' AND image_type='thumb'`)
	if _, err := svc.BackfillPlatformImageHashes(context.Background(), fake, fastOpts); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != platKey("keep", "thumb") {
		t.Errorf("fetched %v, want only the eligible artist", fake.calls)
	}
	if p, c := hashesOf(t, db, "keep", "thumb"); p != "keepme0000000000" || c == "" {
		t.Errorf("hashes = %q/%q, want kept phash and filled content hash", p, c)
	}
	if p, c := hashesOf(t, db, "keep", "logo"); p != "" || c != "" {
		t.Errorf("exists_flag=0 logo written %q/%q", p, c)
	}
}

// Canceling inside a fetch stops the pass after that fetch; the row is not put on cooldown.
func TestBackfillPlatformImageHashes_Cancellation(t *testing.T) {
	svc, db, _, fake := newPass(t)
	seed(t, db, fake, "a", "", "emby", "thumb")
	seed(t, db, fake, "b", "", "emby", "thumb")
	ctx, cancel := context.WithCancel(context.Background())
	fake.onFetch = func(string) { cancel() }
	if _, err := svc.BackfillPlatformImageHashes(ctx, fake, fastOpts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("fetched %v, want exactly one", fake.calls)
	}
	if _, cooling := svc.platformHash.until[platKey("a", "thumb")]; cooling {
		t.Error("the canceled row was put on cooldown")
	}
}

// A local file (default or custom name) is hashed, never fetched, and spends no token.
func TestBackfillPlatformImageHashes_LocalFileIsAuthoritative(t *testing.T) {
	svc, db, cache, fake := newPass(t)
	local := backfillJPEG(t, 50)
	for id, name := range map[string]string{"dflt": "folder.jpg", "cust": "cover.jpg"} {
		seed(t, db, fake, id, "", "emby", "thumb")
		if err := os.MkdirAll(filepath.Join(cache, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, id, name), local, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := PlatformHashBackfillOpts{RequestsPerMinute: 1, Names: func(context.Context, string) ([]string, error) {
		return []string{"cover.jpg"}, nil
	}}
	start := time.Now() // at 1/min a single fetch token would stall the pass
	res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, opts)
	if err != nil || res.Filled != 2 || res.Selected != 0 || len(fake.calls) != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("result = %+v, %v, fetched %v; want 2 filled from local files, no fetch", res, err, fake.calls)
	}
	for _, id := range []string{"dflt", "cust"} {
		if _, c := hashesOf(t, db, id, "thumb"); c != img.ContentHash(local) {
			t.Errorf("%s content hash = %q, want the local file's", id, c)
		}
	}
}

// Fail CLOSED: an unreadable directory or profile means nothing is fetched or written.
func TestBackfillPlatformImageHashes_UnverifiableFailsClosed(t *testing.T) {
	svc, db, cache, fake := newPass(t)
	seed(t, db, fake, "dir", "", "emby", "thumb")
	seed(t, db, fake, "prof", "", "emby", "logo")
	// A regular file where the artist directory belongs: probing it is ENOTDIR, not "absent".
	if err := os.WriteFile(filepath.Join(cache, "dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := fastOpts
	opts.Names = func(_ context.Context, typ string) ([]string, error) {
		if typ == "logo" {
			return nil, errors.New("profile unreadable")
		}
		return nil, nil
	}
	res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, opts)
	if err != nil || res.Failed != 2 || len(fake.calls) != 0 {
		t.Fatalf("result = %+v, %v, fetched %v; want 2 failures and no fetch", res, err, fake.calls)
	}
	for id, typ := range map[string]string{"dir": "thumb", "prof": "logo"} {
		if p, c := hashesOf(t, db, id, typ); p != "" || c != "" {
			t.Errorf("%s written %q/%q", id, p, c)
		}
	}
}

// A hash landing after the drain wins and counts as changed ("full" races a phash writer, "half" a content one).
func TestBackfillPlatformImageHashes_ConcurrentWriteWins(t *testing.T) {
	svc, db, _, fake := newPass(t)
	seed(t, db, fake, "full", "", "emby", "thumb")
	seed(t, db, fake, "half", "", "emby", "thumb")
	exec(t, db, `UPDATE artist_images SET phash='seeded0000000000' WHERE artist_id='half'`)
	race := map[string]string{
		platKey("full", "thumb"): `UPDATE artist_images SET phash='fresh00000000000' WHERE artist_id='full'`,
		platKey("half", "thumb"): `UPDATE artist_images SET content_hash='racedcontent' WHERE artist_id='half'`,
	}
	fake.onFetch = func(k string) { exec(t, db, race[k]) }
	res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, fastOpts)
	if err != nil || res.Changed != 2 || res.Filled != 0 || res.Failed != 0 {
		t.Fatalf("result = %+v, %v; want 2 changed", res, err)
	}
	if p, c := hashesOf(t, db, "full", "thumb"); p != "fresh00000000000" || c != "" {
		t.Errorf("full: concurrent hash clobbered: %q/%q", p, c)
	}
	if p, c := hashesOf(t, db, "half", "thumb"); p != "seeded0000000000" || c != "racedcontent" {
		t.Errorf("half: concurrent content_hash clobbered: %q/%q", p, c)
	}
}

// Failing rows must not hold the cap forever, and are retried after the cooldown.
func TestBackfillPlatformImageHashes_CooldownNoStarvation(t *testing.T) {
	svc, db, _, fake := newPass(t)
	for _, id := range []string{"a1", "a2", "a3", "z-good"} {
		seed(t, db, fake, id, "", "emby", "thumb")
	}
	for _, id := range []string{"a1", "a2", "a3"} {
		delete(fake.slots, platKey(id, "thumb")) // the platform holds nothing for these
	}
	run := func(opts PlatformHashBackfillOpts) *PlatformHashBackfillResult {
		res, err := svc.BackfillPlatformImageHashes(context.Background(), fake, opts)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	capped := PlatformHashBackfillOpts{RequestsPerMinute: 60000, MaxPerPass: 3}
	if res := run(capped); !res.Truncated || res.Selected != 3 {
		t.Errorf("result = %+v, want the cap of 3 to bind", res)
	}
	run(capped)
	if p, _ := hashesOf(t, db, "z-good", "thumb"); p == "" {
		t.Errorf("healthy row not reached within 2 passes; fetch log %v", fake.calls)
	}
	failed := 0
	for range platformFailureCooldownPasses + 1 {
		failed += run(fastOpts).Failed
	}
	if failed != 3 {
		t.Errorf("failing rows re-attempted %d times over the cooldown window, want exactly one retry round (3)", failed)
	}
}

// The scheduler passes after the delay, again per tick, and stops with its context.
func TestStartPlatformHashBackfill_TicksAndStops(t *testing.T) {
	svc, db, _, fake := newPass(t)
	seed(t, db, fake, "t1", "", "emby", "thumb")
	fake.slots[platKey("t2", "thumb")] = backfillJPEG(t, 99) // before the scheduler reads the map
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.StartPlatformHashBackfill(ctx, fake, fastOpts, 40*time.Millisecond, 10*time.Millisecond)
		close(done)
	}()
	waitHashed := func(id string) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if _, c := hashesOf(t, db, id, "thumb"); c != "" {
				return
			}
		}
		t.Fatalf("%s never hashed by the scheduler", id)
	}
	waitHashed("t1")
	seed(t, db, fake, "t2", "", "emby", "thumb")
	waitHashed("t2")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop on context cancel")
	}
}
