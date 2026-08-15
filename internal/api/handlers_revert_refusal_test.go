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
// None is history-tracked today, which is why the third check is dormant;
// TestRevertProbeValues_CoverKnownRules uses it to prove revertProbeValues
// actually trips each of those rules.
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
// On this base no field is BOTH history-tracked AND carrying a validation
// rule, so the check is dormant: reachable only once a later change makes a
// validated field trackable. The test asserts that state positively rather
// than skipping, so the dormancy is a checked precondition rather than an
// assumption, and it runs the real end-to-end assertion the moment the
// precondition stops holding.
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
