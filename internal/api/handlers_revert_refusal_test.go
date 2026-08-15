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

// validatedTrackableField reports a history-tracked field whose OLD VALUE
// artist.ValidateFieldUpdate can refuse, or "" when no such field exists.
//
// It exists because the answer decides which level the refusal path can be
// tested at, and that answer is a property of two lists that live in another
// package rather than something a test may assume. Reading it at run time
// means the tests below keep testing the real code when either list changes,
// instead of quietly testing the wrong arm.
//
// "" is probed as the old value because it is the value a revert writes back
// through ClearField, and it is the value the anticipated first case (a "name"
// change whose old value was empty) carries.
func validatedTrackableField() string {
	for _, f := range artist.TrackableFields() {
		if artist.ValidateFieldUpdate(f, "") != nil {
			return f
		}
	}
	return ""
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
func TestValidateRevertable_OldValueCheck(t *testing.T) {
	t.Parallel()

	field := validatedTrackableField()
	if field == "" {
		// Precondition: every tracked field accepts an empty old value, which
		// is what makes the third check dormant and the unit-level tests below
		// the only way to exercise it.
		for _, f := range artist.TrackableFields() {
			if err := artist.ValidateFieldUpdate(f, ""); err != nil {
				t.Fatalf("ValidateFieldUpdate(%q, \"\") = %v, want nil", f, err)
			}
		}
		return
	}

	change := &artist.MetadataChange{Field: field, Source: "manual", OldValue: ""}
	err := validateRevertable(change)
	if !errors.Is(err, errRevertInvalidOldValue) {
		t.Fatalf("validateRevertable(%q) = %v, want errRevertInvalidOldValue", field, err)
	}
	// The wrapped sentence must be the validator's curated Reason.
	want := fieldRefusalReason(artist.ValidateFieldUpdate(field, ""))
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
