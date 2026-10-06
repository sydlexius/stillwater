package artist

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// captureHandler records every slog record so a test can assert on fields.
type captureHandler struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, r)
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// warnsFor returns the "no producer set" attrs logged for artistID.
func warnsFor(recs []slog.Record, artistID string) []map[string]string {
	var out []map[string]string
	for _, r := range recs {
		if !strings.Contains(r.Message, "no producer set") {
			continue
		}
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if attrs["artist_id"] == artistID {
			out = append(out, attrs)
		}
	}
	return out
}

// TestRecord_WarnsWhenNoProducerSet drives the real HistoryService write path
// on real SQLite (#3078). It swaps the process-wide slog default, so it must
// not run in parallel.
func TestRecord_WarnsWhenNoProducerSet(t *testing.T) {
	svc, db := setupHistoryTestDB(t)
	var recs []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &sync.Mutex{}, recs: &recs}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "SECRET-FIELD-VALUE"
	seedTestArtist(t, db, "warn-unset")
	seedTestArtist(t, db, "warn-explicit")
	seedTestArtist(t, db, "warn-overlay")

	// Nobody set anything: must warn, with artist_id/field/source and no value.
	if err := svc.Record(context.Background(), "warn-unset", "biography", "old", secret, "manual"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got := warnsFor(recs, "warn-unset")
	if len(got) != 1 {
		t.Fatalf("want exactly 1 warn for the unstamped write, got %d", len(got))
	}
	if got[0]["field"] != "biography" || got[0]["source"] != "manual" {
		t.Errorf("warn attrs = %v, want field=biography source=manual", got[0])
	}
	if len(got[0]) != 3 {
		t.Errorf("warn attr keys = %v, want exactly artist_id, field, source", got[0])
	}
	for k, v := range got[0] {
		if strings.Contains(v, secret) {
			t.Errorf("warn attr %s=%q leaks a field value", k, v)
		}
	}

	// Explicit "unrecorded" is a decision, not an omission: no warn, "" stored.
	explicit := ContextWithProducer(context.Background(), ProducerUnrecorded)
	if err := svc.Record(explicit, "warn-explicit", "biography", "old", "new", "manual"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// An overlay naming the field also counts as set.
	overlay := ContextWithFieldProducers(context.Background(), map[string]string{"biography": "provider:lastfm"})
	if err := svc.Record(overlay, "warn-overlay", "biography", "old", "new", "manual"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if n := len(warnsFor(recs, "warn-explicit")) + len(warnsFor(recs, "warn-overlay")); n != 0 {
		t.Errorf("stamped writes warned %d times, want 0", n)
	}
	rows, _, err := svc.List(context.Background(), "warn-explicit", 10, 0)
	if err != nil || len(rows) != 1 || rows[0].Producer != ProducerUnrecorded {
		t.Errorf("explicit-unrecorded row = %+v err=%v, want one row with empty producer", rows, err)
	}
}

// TestRecordHistoryTx_WarnsWhenNoProducerSet pins the same WARN on the
// transactional write path (recordHistoryTx), which has its own resolver call.
func TestRecordHistoryTx_WarnsWhenNoProducerSet(t *testing.T) {
	db := newTestDB(t)
	seedTestArtist(t, db, "warn-tx")
	var recs []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &sync.Mutex{}, recs: &recs}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := recordHistoryTx(context.Background(), tx, "warn-tx", "biography", "old", "new", "revert"); err != nil {
		t.Fatalf("recordHistoryTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got := warnsFor(recs, "warn-tx")
	if len(got) != 1 || len(got[0]) != 3 || got[0]["field"] != "biography" || got[0]["source"] != "revert" {
		t.Fatalf("tx warn attrs = %v, want exactly one warn with artist_id, field=biography, source=revert", got)
	}
}
