package api

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestComputeViolationTally_DBError_Fallback exercises the dbErr fallback path
// of computeViolationTally. When CountActiveViolationsBySeverity fails (here we
// force the failure by closing the shared DB the rule service queries), the
// helper must log a warning and fall back to found-minus-auto-fixed, clamped at
// zero, while passing found/auto-fixed through unchanged.
func TestComputeViolationTally_DBError_Fallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		found         int
		autoFixed     int
		wantRemaining int
	}{
		{
			name:          "found greater than auto-fixed",
			found:         10,
			autoFixed:     4,
			wantRemaining: 6,
		},
		{
			name:          "auto-fixed greater than found clamps to zero",
			found:         3,
			autoFixed:     7,
			wantRemaining: 0,
		},
		{
			name:          "equal counts yield zero",
			found:         5,
			autoFixed:     5,
			wantRemaining: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, _ := testRouter(t)

			// Closing the shared DB forces the rule service's
			// CountActiveViolationsBySeverity query to fail, driving
			// computeViolationTally down its dbErr fallback branch. The router
			// is single-use in this test, so closing its DB is safe.
			if err := r.db.Close(); err != nil {
				t.Fatalf("closing DB to force query error: %v", err)
			}

			result := &rule.RunResult{
				ViolationsFound: tc.found,
				FixesSucceeded:  tc.autoFixed,
			}

			gotFound, gotAutoFixed, gotRemaining := r.computeViolationTally(context.Background(), result)

			if gotFound != tc.found {
				t.Errorf("violationsFound = %d, want %d", gotFound, tc.found)
			}
			if gotAutoFixed != tc.autoFixed {
				t.Errorf("violationsAutoFixed = %d, want %d", gotAutoFixed, tc.autoFixed)
			}
			if gotRemaining != tc.wantRemaining {
				t.Errorf("violationsRemaining = %d, want %d (fallback found-minus-auto-fixed, clamped)", gotRemaining, tc.wantRemaining)
			}
		})
	}
}
