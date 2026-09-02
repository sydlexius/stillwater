package publish

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// #3177: the fanart push path warns, once per push per artist, when the
// artist's extrafanart/ subdirectory is non-empty. These tests drive the real
// SyncAllFanartToPlatforms path (via overCapSyncHarness, which touches
// nothing on disk in its uploader), never a fake enumeration.

// extrafanartWarningHarness wires a Publisher with n connections and a
// stubIndexedUploader that succeeds and touches nothing on disk, so a test
// can prove the warning count is independent of connection count.
func extrafanartWarningHarness(t *testing.T, connCount int) (*Publisher, *artist.Artist, string, *stubIndexedUploader) {
	t.Helper()
	dir := t.TempDir()

	up := &stubIndexedUploader{}
	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

	conns := make(map[string]*connection.Connection, connCount)
	var ids []artist.PlatformID
	for i := 0; i < connCount; i++ {
		id := "c" + string(rune('1'+i))
		conns[id] = &connection.Connection{
			ID: id, Name: "Peer" + string(rune('1'+i)), Type: connection.TypeEmby, Enabled: true, Status: "ok",
			URL:  "http://peer.invalid",
			Emby: &connection.EmbyConfig{FeatureImageWrite: true},
		}
		ids = append(ids, artist.PlatformID{ArtistID: "a1", ConnectionID: id, PlatformArtistID: "p" + string(rune('1'+i))})
	}

	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: ids},
		ConnectionService: &fakeConnectionGetter{conns: conns},
		Logger:            silentLogger(),
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, dir, up
}

// singlePeerHarness wires a Publisher with ONE enabled, write-capable Emby
// peer served by the caller's uploader, logging to logger, and returns the
// artist plus its (empty) directory so the caller can seed a fixture. Every
// #3177 push test differs only in the uploader it injects, so the wiring lives
// here once.
func singlePeerHarness(t *testing.T, up connection.IndexedImageUploader, logger *slog.Logger) (*Publisher, *artist.Artist, string) {
	t.Helper()
	dir := t.TempDir()

	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

	conn := &connection.Connection{
		ID: "c1", Name: "Peer1", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL: "http://peer.invalid",
		// The reconciler path passes respectWriteGate=true, so this must be on
		// for a push to reach the peer there as well as on the interactive path.
		Emby: &connection.EmbyConfig{FeatureImageWrite: true},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c1": conn}},
		Logger:            logger,
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, dir
}

// seedExtrafanart writes n distinct image files under dir/extrafanart/ and
// asserts the fixture's precondition (the exact file count on disk) before
// returning, per the #3177 verification standard.
func seedExtrafanart(t *testing.T, dir string, n int) {
	t.Helper()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir extrafanart: %v", err)
	}
	for i := 0; i < n; i++ {
		base := "extra" + string(rune('1'+i)) + ".jpg"
		name := filepath.Join(sub, base)
		// DISTINCT content per file, not one shared literal. A fixture whose
		// files are byte-identical cannot distinguish "the peer received a
		// different one of them" from "the peer received the right one", so
		// it does not differ along the axis these tests measure.
		if err := os.WriteFile(name, extrafanartBytes(base), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatalf("precondition ReadDir(%s): %v", sub, err)
	}
	if len(entries) != n {
		t.Fatalf("fixture precondition: extrafanart/ holds %d entries, want %d", len(entries), n)
	}
}

// extrafanartBytes is the content seedExtrafanart writes for a given file
// name -- distinct per file, so an assertion about which bytes a peer
// received identifies the FILE and not merely the directory.
func extrafanartBytes(base string) []byte {
	return []byte("extra-fanart-bytes-for-" + base)
}

// primaryFanartBytes is the exact content seedPrimaryFanart writes, so a test
// can POSITIVELY identify what the peer received rather than asserting only
// what it did not receive.
var primaryFanartBytes = []byte("PRIMARY-FANART-BYTES")

// seedPrimaryFanart writes a primary fanart.jpg so DiscoverFanart finds a
// non-empty top-level set -- SyncAllFanartToPlatforms returns early with no
// warning at all when the top-level set is empty, which would make every
// test in this file pass vacuously regardless of the extrafanart behavior.
func seedPrimaryFanart(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), primaryFanartBytes, 0o600); err != nil {
		t.Fatalf("seeding primary fanart: %v", err)
	}
}

// findExtrafanartWarning returns the first warning mentioning extrafanart, or
// "" if none is present.
func findExtrafanartWarning(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "extrafanart") {
			return w
		}
	}
	return ""
}

// countExtrafanartWarnings returns how many warnings mention extrafanart.
func countExtrafanartWarnings(warnings []string) int {
	n := 0
	for _, w := range warnings {
		if strings.Contains(w, "extrafanart") {
			n++
		}
	}
	return n
}

// TestSyncAllFanart_ExtrafanartPresent_WarnsOncePerArtist is the #3177 AC
// directly: a multi-file extrafanart/ and more than one connection produce
// EXACTLY ONE warning naming the file count, not one per file and not one per
// connection.
func TestSyncAllFanart_ExtrafanartPresent_WarnsOncePerArtist(t *testing.T) {
	p, a, dir, up := extrafanartWarningHarness(t, 3)
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 3)

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if up.callCount() == 0 {
		t.Fatal("precondition failed: no peer was ever reached, so this proves nothing about the push path")
	}
	if got := countExtrafanartWarnings(warnings); got != 1 {
		t.Fatalf("got %d extrafanart warning(s) across 3 connections, want exactly 1: %v", got, warnings)
	}
	// EXACT count phrase, not a bare "3" substring: "13" contains "3", so the
	// looser form survives a len(files)+10 mutation.
	msg := findExtrafanartWarning(warnings)
	if !strings.Contains(msg, "holds 3 file(s)") {
		t.Errorf("warning %q does not name the exact file count (want the phrase \"holds 3 file(s)\")", msg)
	}
}

// TestSyncAllFanart_ExtrafanartAbsent_NoWarning is the negative case: an
// artist with no extrafanart/ directory at all gets no such warning.
func TestSyncAllFanart_ExtrafanartAbsent_NoWarning(t *testing.T) {
	p, a, dir, up := extrafanartWarningHarness(t, 1)
	seedPrimaryFanart(t, dir)
	// Deliberately no extrafanart/ directory created.

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if up.callCount() == 0 {
		t.Fatal("precondition failed: no peer was ever reached")
	}
	if msg := findExtrafanartWarning(warnings); msg != "" {
		t.Errorf("got extrafanart warning %q for an artist with no extrafanart/ directory", msg)
	}
}

// TestSyncAllFanart_ExtrafanartEmpty_NoWarning covers the directory existing
// but holding nothing.
func TestSyncAllFanart_ExtrafanartEmpty_NoWarning(t *testing.T) {
	p, a, dir, up := extrafanartWarningHarness(t, 1)
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 0)

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if up.callCount() == 0 {
		t.Fatal("precondition failed: no peer was ever reached")
	}
	if msg := findExtrafanartWarning(warnings); msg != "" {
		t.Errorf("got extrafanart warning %q for an empty extrafanart/ directory", msg)
	}
}

// TestSyncAllFanart_ExtrafanartWarning_NeverPushesExtraFiles is the negative
// side of the #3177 scope statement: the enumeration is a NOTICE only. The
// peer receives the top-level primary's EXACT bytes and nothing else, and the
// extrafanart/ files are still on disk byte-identical afterwards.
//
// WHAT THIS TEST DOES AND DOES NOT GUARD, stated plainly because an earlier
// version of it was cited as proof of something stronger. The property that
// extrafanart/ files are not pushed is owned by DiscoverFanart, which lists
// an artist directory's own entries and skips subdirectories -- it held
// before #3177 and #3177 changes nothing about it. So deleting the #3177
// code would leave this test GREEN, and that is correct: it is a REGRESSION
// GUARD on a pre-existing invariant this branch must not disturb, not
// evidence of what the branch itself does. Its assertions are POSITIVE
// (the peer got exactly the primary's bytes, at exactly one index) rather
// than negative (the peer did not get the extra string), because a negative
// substring check also passes when the uploader was handed nothing at all.
func TestSyncAllFanart_ExtrafanartWarning_NeverPushesExtraFiles(t *testing.T) {
	p, a, dir, up := extrafanartWarningHarness(t, 1)
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 2)

	extraContents := map[string][]byte{}
	sub := filepath.Join(dir, "extrafanart")
	for _, name := range []string{"extra1.jpg", "extra2.jpg"} {
		b, err := os.ReadFile(filepath.Join(sub, name))
		if err != nil {
			t.Fatalf("reading fixture %s before push: %v", name, err)
		}
		// Precondition: each seeded file carries DISTINCT content, so an
		// assertion below identifies a specific file rather than merely "some
		// file in extrafanart/".
		if string(b) != string(extrafanartBytes(name)) {
			t.Fatalf("fixture precondition: extrafanart/%s holds %q, want the per-file distinct content %q",
				name, b, extrafanartBytes(name))
		}
		extraContents[name] = b
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	// The push has exactly one top-level fanart file (the seeded primary), so
	// the uploader must have received exactly one call.
	if up.callCount() != 1 {
		t.Fatalf("uploader received %d call(s), want 1 (only the top-level primary)", up.callCount())
	}
	// POSITIVE identification: the peer got the PRIMARY's exact bytes. This
	// fails both when the peer is handed an extrafanart file AND when it is
	// handed nothing, which the previous negative-substring form did not.
	got, ok := up.received(0)
	if !ok {
		t.Fatal("the uploader recorded no payload at index 0; the peer was handed nothing")
	}
	if string(got) != string(primaryFanartBytes) {
		t.Fatalf("the peer received %q at index 0, want the top-level primary's exact bytes %q -- "+
			"#3177 forbids pushing extrafanart/ files and the primary must still be pushed",
			got, primaryFanartBytes)
	}
	for name, want := range extraContents {
		got, err := os.ReadFile(filepath.Join(sub, name))
		if err != nil {
			t.Fatalf("extrafanart file %s is missing after the push -- it was destroyed", name)
		}
		if string(got) != string(want) {
			t.Errorf("extrafanart file %s changed during the push: got %q, want %q", name, got, want)
		}
	}
}

// TestSyncAllFanart_ExtrafanartEnumerationFailure_DoesNotBlockPush is the
// #3177 AC: a failure to enumerate does not fail or skip the push. Simulated
// by replacing extrafanart/ with a regular file, so os.ReadDir on it fails
// with something other than "not exist".
func TestSyncAllFanart_ExtrafanartEnumerationFailure_DoesNotBlockPush(t *testing.T) {
	p, a, dir, up := extrafanartWarningHarness(t, 1)
	seedPrimaryFanart(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "extrafanart"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seeding non-directory extrafanart: %v", err)
	}

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if up.callCount() == 0 {
		t.Fatal("the push was skipped after an extrafanart enumeration failure -- #3177 forbids that; " +
			"a failure to enumerate must not fail or skip the push")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "extrafanart") && strings.Contains(w, "could not") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning reported the enumeration failure; silently swallowing it is forbidden. got: %v", warnings)
	}
}

// TestSyncAllFanart_TimestampStampedBeforeExtrafanartCheck pins the #3177 AC
// that the #2712 push timestamp is still stamped as the FIRST statement of
// syncAllFanartToPlatforms -- no new work, including this enumeration, is
// inserted ahead of it. This is verified structurally (source order), which
// is what the AC calls for ("a test or a review note must confirm this
// explicitly"): a behavioral test cannot observe an unexported struct field's
// write order without a seam this package does not otherwise need.
func TestSyncAllFanart_TimestampStampedBeforeExtrafanartCheck(t *testing.T) {
	src, err := os.ReadFile("publisher.go")
	if err != nil {
		t.Fatalf("reading publisher.go: %v", err)
	}
	fnStart := strings.Index(string(src), "func (p *Publisher) syncAllFanartToPlatforms(")
	if fnStart < 0 {
		t.Fatal("syncAllFanartToPlatforms not found in publisher.go")
	}
	body := string(src)[fnStart:]

	stampIdx := strings.Index(body, "push := pushScope{at: time.Now()}")
	if stampIdx < 0 {
		t.Fatal("the #2712 timestamp statement was not found verbatim in syncAllFanartToPlatforms")
	}
	warnIdx := strings.Index(body, "p.extrafanartExposureWarning(")
	if warnIdx < 0 {
		t.Fatal("extrafanartExposureWarning call not found in syncAllFanartToPlatforms")
	}
	if warnIdx < stampIdx {
		t.Fatalf("extrafanartExposureWarning (offset %d) appears BEFORE the #2712 timestamp stamp (offset %d); "+
			"the timestamp must remain the first statement of the function", warnIdx, stampIdx)
	}

	// Also pin that nothing NEW sits between the function's opening brace and
	// the stamp beyond the pre-existing `if p == nil { return nil }` guard.
	// Skip blank lines, full-line comments, and that one guard's three lines
	// (its own multi-line form); the next non-skipped, non-blank, non-comment
	// line must be the stamp. This is the AC's actual invariant -- "no new
	// work is inserted ahead of it" -- the guard predates #3177 and is not
	// new work.
	braceIdx := strings.Index(body, "{")
	if braceIdx < 0 {
		t.Fatal("could not find the function's opening brace")
	}
	skippable := map[string]bool{
		"if p == nil {": true,
		"return nil":    true,
		"}":             true,
	}
	sawStamp := false
	for _, line := range strings.Split(body[braceIdx+1:], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || skippable[trimmed] {
			continue
		}
		sawStamp = trimmed == "push := pushScope{at: time.Now()}"
		break
	}
	if !sawStamp {
		t.Fatal("the #2712 timestamp is not the first statement of syncAllFanartToPlatforms " +
			"beyond the pre-existing nil guard (some new statement precedes it)")
	}
}

// TestReconciler_ExtrafanartWarning_ReachesTheLogOnly pins round 2's MAJOR-1:
// the operator-facing docs page states, precisely, that the BACKGROUND
// reconciliation pass warns to Stillwater's LOG rather than on screen. That is
// a real limitation, not an accident -- syncMissingArtwork is the only consumer
// of syncAllFanartToPlatforms' warnings on that path and it returns nothing, so
// there is no channel by which a toast or an HX-Trigger could carry it.
//
// The docs sentence was false twice before this test existed (it promised the
// warning unconditionally while the reconciler path was silent). It is pinned
// here so that either half changing -- the reconciler gaining an operator
// surface, or the warning ceasing to be logged at all -- reddens a test and
// forces the docs page to be changed with it.
//
// It asserts BOTH directions, so it cannot pass vacuously:
//   - the reconciler push really did happen (a peer was reached), and
//   - the extrafanart notice really is present in the log records.
func TestReconciler_ExtrafanartWarning_ReachesTheLogOnly(t *testing.T) {
	var logBuf bytes.Buffer
	up := &stubIndexedUploader{}
	p, a, dir := singlePeerHarness(t, up,
		slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 3)

	// The reconciler's own entry point for a fanart-needing artist. It returns
	// NOTHING: that absent return value is the limitation the docs describe.
	p.syncMissingArtwork(context.Background(), a, artworkNeeds{fanart: true})

	if up.callCount() == 0 {
		t.Fatal("precondition failed: the reconciler push reached no peer, so no fanart push ran and this " +
			"test is not exercising the path it claims to")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "extrafanart") {
		t.Fatalf("the reconciler's fanart push raised no extrafanart notice in the log. The docs page tells "+
			"operators that the background pass's warning appears in the log; if that is no longer true, "+
			"change docs/site/src/core-concepts/images.md with this code. Log was:\n%s", logged)
	}
	if !strings.Contains(logged, "3 file(s)") {
		t.Fatalf("the logged reconciler notice does not name the 3-file count the fixture holds. Log was:\n%s", logged)
	}
}
