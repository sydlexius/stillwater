package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// revertProbeValues are the candidate OLD VALUES the dormancy probe below
// offers to every history-tracked field. They are curated literals and stay
// literals -- nothing dynamic is interpolated -- because their job is to be a
// fixed, reviewable sample of the value SHAPES artist's rules refuse: "" and
// " " for "name cannot be empty", "-" and "\u200b" (a zero-width space, an
// escape so a reader can see it) for the empty-identity-key rule, and the two
// non-UUID strings for the "musicbrainz_id" shape rule. Probing only "" was
// the defect this replaces: "musicbrainz_id" accepts "" (a clear is
// legitimate) while refusing "not-a-uuid", so a single-value probe called it
// unvalidated.
var revertProbeValues = []string{"", " ", "-", "\u200b", "not-a-uuid", "12345"}

// knownValidatedFields lists every field artist.ValidateFieldUpdate polices.
// "name" is history-tracked as of #3037, which is what makes the third check
// LIVE; TestRevertProbeValues_CoverKnownRules uses this list to prove
// revertProbeValues actually trips each of those rules.
var knownValidatedFields = []string{"name", "musicbrainz_id"}

// validatedTrackableField reports a history-tracked field whose OLD VALUE
// artist.ValidateFieldUpdate can refuse, together with a probe value it
// refuses, or ("", "") when no tracked field refuses any of revertProbeValues.
//
// It exists because the answer decides which level the refusal path can be
// tested at, and that answer is a property of two lists that live in another
// package rather than something a test may assume. Reading it at run time
// means the tests below keep testing the real code when either list changes,
// instead of quietly testing the wrong arm.
//
// LIMIT OF THE PROBE, so the next reader does not over-trust it: a rule added
// to a field NOT in knownValidatedFields, refusing only values outside
// revertProbeValues, still reads as unvalidated here. The guard is
// TestRevertProbeValues_CoverKnownRules plus keeping both lists current.
func validatedTrackableField() (field, oldValue string) {
	for _, f := range artist.TrackableFields() {
		for _, v := range revertProbeValues {
			if artist.ValidateFieldUpdate(f, v) != nil {
				return f, v
			}
		}
	}
	return "", ""
}

// TestRevertProbeValues_CoverKnownRules fails loudly when revertProbeValues
// stops representing a rule artist.ValidateFieldUpdate actually enforces.
//
// It is the honesty check under the dormancy assertion below, which reads "no
// tracked field refuses any probe value" -- evidence of dormancy only if the
// probe values are known to trip the rules that exist. A rule whose refused
// values are unrepresented would make the probe call a validated field
// unvalidated: silently passing while asserting something false.
func TestRevertProbeValues_CoverKnownRules(t *testing.T) {
	t.Parallel()

	for _, f := range knownValidatedFields {
		refused := false
		for _, v := range revertProbeValues {
			if artist.ValidateFieldUpdate(f, v) != nil {
				refused = true
				break
			}
		}
		if !refused {
			t.Errorf("no value in revertProbeValues is refused for field %q; "+
				"its validation rule changed shape, so add a value the current rule "+
				"refuses (and add the field to knownValidatedFields if a NEW field "+
				"gained a rule), or the dormancy probe will report it as unvalidated", f)
		}
	}
}

// TestValidateRevertable_OldValueCheck covers validateRevertable's third check
// against whatever the two lists actually say right now.
//
// The test does not hardcode which arm it takes. When it landed, no field was
// BOTH history-tracked AND carrying a validation rule, so the check was
// dormant and this asserted that state positively rather than skipping. #3037
// added "name" to trackableFields, and "name" carries the empty-name and
// empty-identity-key rules -- so the probe below now returns a real field and
// this test takes the LIVE end-to-end arm instead. Both arms are kept: the
// dormancy arm is what makes the live arm's precondition honest rather than
// assumed, and it would be correct again if the last validated field ever left
// trackableFields.
//
// Dormancy is decided by offering every tracked field every value in
// revertProbeValues, not just "". A field can accept "" and still refuse other
// recorded old values -- "musicbrainz_id" is that shape -- so a probe of ""
// alone would report "dormant" for a check that is live and rejecting real
// history rows.
func TestValidateRevertable_OldValueCheck(t *testing.T) {
	t.Parallel()

	field, oldValue := validatedTrackableField()
	if field == "" {
		// Precondition: no tracked field refuses any probe value, which is what
		// makes the third check dormant and the unit-level tests below the only
		// way to exercise it.
		for _, f := range artist.TrackableFields() {
			for _, v := range revertProbeValues {
				if err := artist.ValidateFieldUpdate(f, v); err != nil {
					t.Fatalf("ValidateFieldUpdate(%q, %q) = %v, want nil", f, v, err)
				}
			}
		}
		return
	}

	change := &artist.MetadataChange{Field: field, Source: "manual", OldValue: oldValue}
	err := validateRevertable(change)
	if !errors.Is(err, errRevertInvalidOldValue) {
		t.Fatalf("validateRevertable(%q, old=%q) = %v, want errRevertInvalidOldValue",
			field, oldValue, err)
	}
	// The wrapped sentence must be the validator's curated Reason.
	want := fieldRefusalReason(artist.ValidateFieldUpdate(field, oldValue))
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not carry the refusal reason %q", err, want)
	}
}

func TestFieldRefusalReason(t *testing.T) {
	t.Parallel()

	generic := artist.ErrInvalidFieldValue.Error()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "typed refusal yields its curated reason",
			err:  &artist.FieldValidationError{Field: "name", Reason: "name cannot be empty"},
			want: "name cannot be empty",
		},
		{
			name: "wrapped typed refusal is still unwrapped to the reason",
			err: fmt.Errorf("updating field: %w",
				&artist.FieldValidationError{Field: "name", Reason: "name cannot be empty"}),
			want: "name cannot be empty",
		},
		{
			name: "typed refusal with no reason falls back to the sentinel",
			err:  &artist.FieldValidationError{Field: "name"},
			want: generic,
		},
		{
			name: "an error of unknown provenance is never shown",
			err:  errors.New("pq: column \"name\" violates constraint on artist 42"),
			want: generic,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fieldRefusalReason(tc.err); got != tc.want {
				t.Errorf("fieldRefusalReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteRevertFailure_Classification pins the status code and the
// client-visible body for each arm of the classifier. The value-refusal arm is
// the one #3037 adds: without it the refusal falls through to a 500, telling
// the operator a server fault occurred for a request that was refused on
// purpose and will be refused identically on every retry.
func TestWriteRevertFailure_Classification(t *testing.T) {
	t.Parallel()

	r, _, _ := testRouterWithHistory(t)
	change := &artist.MetadataChange{ID: "c1", ArtistID: "a1", Field: "name", OldValue: ""}

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantBody    string
		notWantBody string
	}{
		{
			name:       "a missing artist is a 404",
			err:        fmt.Errorf("loading artist: %w", artist.ErrNotFound),
			wantStatus: http.StatusNotFound,
			wantBody:   "artist not found",
		},
		{
			name: "a refused value is a 400 naming the reason",
			err: fmt.Errorf("writing field: %w",
				&artist.FieldValidationError{Field: "name", Reason: "name cannot be empty"}),
			wantStatus: http.StatusBadRequest,
			wantBody:   errRevertInvalidOldValue.Error() + ": name cannot be empty",
		},
		{
			name: "a refusal of unknown provenance leaks nothing",
			err: fmt.Errorf("writing field: %w: artist a1 column name: driver said no",
				artist.ErrInvalidFieldValue),
			wantStatus:  http.StatusBadRequest,
			wantBody:    artist.ErrInvalidFieldValue.Error(),
			notWantBody: "driver said no",
		},
		{
			name: "a name collision is a 409 naming the colliding artist",
			err: fmt.Errorf("writing field: %w", &artist.NameCollisionError{
				Collision: &artist.NameCollision{ArtistID: "a2", Name: "Northfield Chorale"},
			}),
			wantStatus: http.StatusConflict,
			wantBody:   "Northfield Chorale",
		},
		{
			// The nil guard, which is why the branch tests Collision != nil
			// rather than trusting the type: writeNameCollisionRefusal
			// dereferences that pointer, so reaching it with a nil would panic
			// the handler. Falling through to the 500 is the correct answer.
			name:       "a collision error with no colliding artist falls through to 500",
			err:        fmt.Errorf("writing field: %w", &artist.NameCollisionError{}),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "revert failed",
		},
		{
			name:       "anything else is still a 500",
			err:        errors.New("database is locked"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "revert failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/history/c1/revert", nil)
			w := httptest.NewRecorder()

			r.writeRevertFailure(w, req, "c1", change, tc.err)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not contain %q", w.Body.String(), tc.wantBody)
			}
			if tc.notWantBody != "" && strings.Contains(w.Body.String(), tc.notWantBody) {
				t.Errorf("body %q leaked %q", w.Body.String(), tc.notWantBody)
			}
		})
	}
}

// TestRevertName_IsReachableAndStillGuarded is the affordance check for #3037.
//
// Adding "name" to trackableFields is the only change in this series that
// TURNS SOMETHING ON: IsTrackableField("name") becomes true, so the per-field
// Undo renders and the revert endpoint stops answering 400 "not revertible".
// The risk that creates is that a newly-reachable write path bypasses a guard
// installed earlier, so this asserts both halves of the intended behavior --
// the affordance WORKS, and it is STILL GUARDED -- through the real handler.
func TestRevertName_IsReachableAndStillGuarded(t *testing.T) {
	t.Parallel()

	// PRECONDITION: without this the two arms below could both pass for the
	// wrong reason (a "not revertible" 400 looks like a refusal too).
	if !artist.IsTrackableField("name") {
		t.Fatal("precondition: \"name\" is not trackable, so the revert endpoint refuses " +
			"it before either arm below can be reached")
	}

	r, artistSvc, historySvc := testRouterWithHistory(t)
	// Without this wiring the service records no history at all, and the
	// first arm below would fail on an empty change list rather than on the
	// behavior under test.
	artistSvc.SetHistoryService(historySvc)
	occupant := addTestArtist(t, artistSvc, "Northfield Chorale")
	subject := addTestArtist(t, artistSvc, "Southgate Winds")

	revert := func(t *testing.T, changeID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
		req.SetPathValue("id", changeID)
		w := httptest.NewRecorder()
		r.handleRevertHistory(w, req)
		return w
	}

	// changeIDWithOldValue finds the one recorded "name" change carrying the
	// given old value. It selects on CONTENT rather than taking the newest
	// row: metadata_changes stores created_at at SECOND resolution, so rows
	// this test writes within one second tie on the ordering and fall back to
	// comparing random UUIDs, which would make the fixture pick a different
	// row on some runs than on others.
	changeIDWithOldValue := func(t *testing.T, artistID, oldValue string) string {
		t.Helper()
		changes, _, err := historySvc.List(t.Context(), artistID, 100, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var found []string
		for _, c := range changes {
			if c.Field == "name" && c.OldValue == oldValue {
				found = append(found, c.ID)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d recorded name changes with old_value %q, want exactly 1: %+v",
				len(found), oldValue, changes)
		}
		return found[0]
	}

	// THE AFFORDANCE WORKS. A name change to a free name is recorded, and
	// reverting it puts the operator's name back. This is the POSITIVE
	// CONTROL for the refusal below: without it, a handler that refused every
	// name revert would satisfy the refusal assertion vacuously.
	t.Run("a name revert to a free name is applied", func(t *testing.T) {
		if _, err := artistSvc.UpdateField(t.Context(), subject.ID, "name", "Harrowdene Ensemble"); err != nil {
			t.Fatalf("seeding the name change: %v", err)
		}
		w := revert(t, changeIDWithOldValue(t, subject.ID, "Southgate Winds"))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; the Undo affordance #3037 turns on must actually "+
				"work. Body: %s", w.Code, w.Body.String())
		}
		got, err := artistSvc.GetByID(t.Context(), subject.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Name != "Southgate Winds" {
			t.Errorf("name = %q, want %q: the revert reported success without restoring the value",
				got.Name, "Southgate Winds")
		}
	})

	// THE GUARD STILL FIRES. A history row whose old value is a name ANOTHER
	// artist now holds must not be written back: doing so would recreate the
	// duplicate the collision guard exists to prevent. The row is seeded
	// directly because no service path would record a change INTO a colliding
	// state, which is exactly why the guard has to hold at revert time.
	//
	// The status asserted is 409 Conflict, which is what a refused manual
	// rename already returns: the refusal is about the CURRENT state of the
	// database, not a malformed request, and routing it through
	// writeNameCollisionRefusal is what makes the two the same response by
	// construction. Before that arm existed the refusal fell through to a
	// generic 500 "revert failed", which told the operator "server fault" for
	// a request that was refused on purpose and will be refused identically
	// on every retry.
	t.Run("a name revert onto another artist's identity is refused", func(t *testing.T) {
		addHistoryChange(t, historySvc, subject.ID, "name",
			occupant.Name, "Southgate Winds", "rule:name_language_pref")
		w := revert(t, changeIDWithOldValue(t, subject.ID, occupant.Name))
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d: a revert onto an identity another artist "+
				"already holds must be refused as a CONFLICT, not applied (which would "+
				"recreate the duplicate #2730 and #2807 exist to prevent) and not reported "+
				"as a server fault. Body: %s", w.Code, http.StatusConflict, w.Body.String())
		}

		got, err := artistSvc.GetByID(t.Context(), subject.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Name != "Southgate Winds" {
			t.Errorf("name = %q, want %q unchanged: the refused revert wrote anyway",
				got.Name, "Southgate Winds")
		}
		other, err := artistSvc.GetByID(t.Context(), occupant.ID)
		if err != nil {
			t.Fatalf("GetByID occupant: %v", err)
		}
		if other.Name != "Northfield Chorale" {
			t.Errorf("occupant name = %q, want %q unchanged", other.Name, "Northfield Chorale")
		}
	})
}
