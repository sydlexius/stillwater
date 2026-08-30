package rule

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// #3064: selfHealMBID records an adoption the lock guard reverted.
//
// selfHealMBID mutates a.MusicBrainzID and persists through the whole-row
// artist.Service.Update. Since #3037/#3060 that write goes through a
// per-field lock chokepoint (internal/artist/lockguard.go) which RESTORES a
// locked field and CONTINUES rather than refusing -- so a plain Update
// returns nil even when the write never landed. The bare Update this fixer
// used to call could not tell the difference, so recordBulkMBIDHistory wrote
// a false "adopted" audit row and the in-memory artist kept a value the
// database never accepted.
//
// Every "reverted" assertion below is paired with a positive control on an
// otherwise identical UNLOCKED artist that must genuinely adopt the MBID.
// Without that pairing, an assertion that "nothing was recorded" would pass
// whether the guard refused the write or the self-heal path was never
// reached at all (see feedback_vacuous_test_precondition in project memory).

// mbidLockFixture wires a BulkExecutor with a real artist.Service (SQLite,
// so the lockguard chokepoint and history table are both live) and a
// confident, unambiguous MusicBrainz search result for gateArtistName. When
// lockMBID is true, musicbrainz_id is pinned through the lock mutator before
// the self-heal runs -- not by setting LockedFields on the struct, which the
// chokepoint would pin away regardless (see restoreLockedField).
func mbidLockFixture(t *testing.T, lockMBID bool) (*BulkExecutor, *artist.Service, *artist.HistoryService, *artist.Artist) {
	t.Helper()
	results := []provider.ArtistSearchResult{
		{Name: gateArtistName, MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
	}
	e, artistSvc, historySvc, a := newBulkGateExecutor(t, results)
	a.Name = gateArtistName
	// A readable-but-empty album directory keeps the #2858 album gate on its
	// fail-open EvidenceNone branch, so only the lock guard under test can
	// decline this candidate -- assertNameGatesPass below pins the other half
	// (the candidate clears EvaluateMBIDCandidate on its own).
	assertNameGatesPass(t, results)

	if lockMBID {
		if err := artistSvc.AddLockedField(context.Background(), a.ID, "musicbrainz_id"); err != nil {
			t.Fatalf("locking musicbrainz_id: %v", err)
		}
	}
	stored, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// PRECONDITION: the lock state actually persisted (or, in the unlocked
	// case, genuinely did not). Without this the two branches below could
	// silently converge on the same fixture and neither assertion would mean
	// anything.
	locked := false
	for _, f := range stored.LockedFields {
		if f == "musicbrainz_id" {
			locked = true
		}
	}
	if locked != lockMBID {
		t.Fatalf("precondition: musicbrainz_id locked = %v, want %v (locked_fields=%v)", locked, lockMBID, stored.LockedFields)
	}
	if stored.MusicBrainzID != "" {
		t.Fatalf("precondition: artist must start with an empty MBID, got %q", stored.MusicBrainzID)
	}
	*a = *stored
	return e, artistSvc, historySvc, a
}

// mbidHistoryRows returns every recorded musicbrainz_id change for the
// artist, so a test can assert on presence/absence and on field content
// without repeating the List+filter boilerplate.
func mbidHistoryRows(t *testing.T, h *artist.HistoryService, artistID string) []artist.MetadataChange {
	t.Helper()
	changes, _, err := h.List(context.Background(), artistID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var out []artist.MetadataChange
	for _, c := range changes {
		if c.Field == "musicbrainz_id" {
			out = append(out, c)
		}
	}
	return out
}

// TestSelfHealMBID_LockedFieldNotAdopted is the headline regression (#3064):
// a pinned musicbrainz_id must leave the row alone, must not be reflected on
// the in-memory artist, and must write no history row claiming an adoption
// that never happened.
func TestSelfHealMBID_LockedFieldNotAdopted(t *testing.T) {
	// POSITIVE CONTROL FIRST: the identical fixture, unlocked, must actually
	// adopt the MBID. If this fails, selfHealMBID is unreachable in this
	// fixture shape and the locked case below proves nothing.
	e, artistSvc, historySvc, a := mbidLockFixture(t, false)
	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)
	if status != "" {
		t.Fatalf("positive control FAILED: expected the self-heal to be ALLOWED when unlocked, got status %q (message %q)", status, msg)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Fatalf("positive control FAILED: a.MusicBrainzID = %q, want %q; the write under test is unreachable here", a.MusicBrainzID, mbidRadiohead)
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if reloaded.MusicBrainzID != mbidRadiohead {
		t.Fatalf("positive control FAILED: persisted MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, mbidRadiohead)
	}
	if rows := mbidHistoryRows(t, historySvc, a.ID); len(rows) != 1 {
		t.Fatalf("positive control FAILED: musicbrainz_id history rows = %d, want 1 (%+v)", len(rows), rows)
	} else if rows[0].NewValue != mbidRadiohead {
		t.Errorf("positive control: history new_value = %q, want %q", rows[0].NewValue, mbidRadiohead)
	}

	// THE REGRESSION: the same candidate, on an artist with musicbrainz_id
	// PINNED. The persist chokepoint restores the stored (empty) value and
	// CONTINUES -- Update itself would return nil -- so this must be caught by
	// the restored-field report, not by an error return.
	e, artistSvc, historySvc, a = mbidLockFixture(t, true)
	status, msg = e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != BulkItemSkipped {
		t.Fatalf("status = %q, want %q (message: %q); a locked MBID must not report anything but a skip", status, BulkItemSkipped, msg)
	}
	if !strings.Contains(msg, "locked") {
		t.Errorf("message %q does not tell the operator the field is locked", msg)
	}
	// THE IN-MEMORY MIRROR MATTERS MOST. fetchImages (the caller) reads this
	// same *Artist to decide whether to proceed to FetchImages with an
	// identity; a stale claim here would let a reverted write drive a
	// downstream provider fetch under an ID the database never accepted.
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: a lock-reverted adoption must not be mirrored in memory", a.MusicBrainzID)
	}
	if got, ok := a.MetadataSources[artist.SourceKeyMusicBrainzID]; ok {
		t.Errorf("MetadataSources[musicbrainz_id] = %q, want absent: a reverted write must not leave a machine-picked provenance stamp", got)
	}

	reloaded, err = artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "" {
		t.Fatalf("persisted MusicBrainzID = %q, want empty: the lock guard must have kept the stored row untouched", reloaded.MusicBrainzID)
	}

	// THE FALSE-HISTORY-ROW ASSERTION THIS ISSUE IS ABOUT. Before the fix, the
	// bare Update() reported no error, so recordBulkMBIDHistory ran
	// unconditionally and wrote a row claiming an adoption that the guard had
	// just thrown away.
	if rows := mbidHistoryRows(t, historySvc, a.ID); len(rows) != 0 {
		t.Errorf("musicbrainz_id history rows = %d, want 0; a lock-reverted write must not be recorded as an adoption: %+v", len(rows), rows)
	}
}

// TestSelfHealMBID_LockedFieldLogsRevertedWrite pins the operator-visible
// diagnostic: a lock silently absorbing an automated write with no trace
// anywhere is exactly the failure mode #3037's whole file exists to close.
func TestSelfHealMBID_LockedFieldLogsRevertedWrite(t *testing.T) {
	e, _, _, a := mbidLockFixture(t, true)

	// Route the executor's logger through a buffer so the log record can be
	// inspected without depending on stderr capture.
	buf := &bytes.Buffer{}
	e.logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	status, _ := e.selfHealMBID(context.Background(), a, BulkModeYOLO)
	if status != BulkItemSkipped {
		t.Fatalf("precondition: status = %q, want %q", status, BulkItemSkipped)
	}

	logged := buf.String()
	if !strings.Contains(logged, "reverted") {
		t.Errorf("no record of the lock-reverted self-heal in the log. Log was:\n%s", logged)
	}
	if !strings.Contains(logged, a.ID) {
		t.Errorf("the log record does not name the artist. Log was:\n%s", logged)
	}
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("a lock fighting an automated write is exactly the operator-visible conflict #3037 logs at ERROR, "+
			"not a routine skip. Log was:\n%s", logged)
	}
}
