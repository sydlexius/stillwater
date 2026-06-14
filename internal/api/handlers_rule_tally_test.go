package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

			gotFound, gotAutoFixed, gotRemaining := r.computeViolationTally(context.Background(), result, "test-fallback")

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

// TestComputeViolationTally_DBSuccess exercises the happy path of
// computeViolationTally. CountActiveViolationsBySeverity succeeds against the
// live test DB, and the three notif_badge_severity_* settings control which
// severity buckets contribute to violationsRemaining.
func TestComputeViolationTally_DBSuccess(t *testing.T) {
	t.Parallel()

	// insertViolation writes a single active open violation directly into the
	// test DB. FK constraints are disabled in test DBs (database.Open does not
	// call EnableForeignKeys), so arbitrary rule_id and artist_id strings are
	// safe here. artist_id is derived from id to satisfy the UNIQUE(rule_id,
	// artist_id) constraint without needing a real artists row.
	insertViolation := func(t *testing.T, r *Router, id, severity string) {
		t.Helper()
		_, err := r.db.ExecContext(context.Background(), `
			INSERT INTO rule_violations
				(id, rule_id, artist_id, artist_name, severity, message, status, created_at, updated_at)
			VALUES (?, 'rule-tally-test', ?, 'Test Artist', ?, 'test violation', 'open', datetime('now'), datetime('now'))`,
			id, "artist-"+id, severity)
		if err != nil {
			t.Fatalf("insertViolation(%q, %q): %v", id, severity, err)
		}
	}

	cases := []struct {
		name          string
		violations    [][2]string       // (id, severity) pairs to insert
		settings      map[string]string // key -> "true"/"false"; absent = default
		result        *rule.RunResult
		wantFound     int
		wantAutoFixed int
		wantRemaining int
	}{
		{
			name: "all three severities enabled with non-zero counts",
			violations: [][2]string{
				{"v1", "error"},
				{"v2", "warning"},
				{"v3", "info"},
			},
			settings: map[string]string{
				"notif_badge_severity_error":   "true",
				"notif_badge_severity_warning": "true",
				"notif_badge_severity_info":    "true",
			},
			result:        &rule.RunResult{ViolationsFound: 5, FixesSucceeded: 2},
			wantFound:     5,
			wantAutoFixed: 2,
			wantRemaining: 3, // 1 error + 1 warning + 1 info
		},
		{
			name: "info disabled by default excludes info count",
			violations: [][2]string{
				{"v1", "error"},
				{"v2", "warning"},
				{"v3", "info"},
			},
			// No settings inserted: defaults are error=true, warning=true, info=false.
			settings:      nil,
			result:        &rule.RunResult{ViolationsFound: 10, FixesSucceeded: 1},
			wantFound:     10,
			wantAutoFixed: 1,
			wantRemaining: 2, // 1 error + 1 warning; info excluded by default
		},
		{
			name: "error disabled mid-table excludes its count",
			violations: [][2]string{
				{"v1", "error"},
				{"v2", "warning"},
				{"v3", "info"},
			},
			settings: map[string]string{
				"notif_badge_severity_error":   "false",
				"notif_badge_severity_warning": "true",
				"notif_badge_severity_info":    "true",
			},
			result:        &rule.RunResult{ViolationsFound: 3, FixesSucceeded: 0},
			wantFound:     3,
			wantAutoFixed: 0,
			wantRemaining: 2, // 1 warning + 1 info; error excluded
		},
		{
			name:          "zero violations yields zero remaining",
			violations:    nil,
			settings:      nil,
			result:        &rule.RunResult{ViolationsFound: 0, FixesSucceeded: 0},
			wantFound:     0,
			wantAutoFixed: 0,
			wantRemaining: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, _ := testRouter(t)

			for _, v := range tc.violations {
				insertViolation(t, r, v[0], v[1])
			}
			for k, v := range tc.settings {
				insertSetting(t, r, k, v)
			}

			gotFound, gotAutoFixed, gotRemaining := r.computeViolationTally(context.Background(), tc.result, "test-success")

			if gotFound != tc.wantFound {
				t.Errorf("violationsFound = %d, want %d", gotFound, tc.wantFound)
			}
			if gotAutoFixed != tc.wantAutoFixed {
				t.Errorf("violationsAutoFixed = %d, want %d", gotAutoFixed, tc.wantAutoFixed)
			}
			if gotRemaining != tc.wantRemaining {
				t.Errorf("violationsRemaining = %d, want %d (severity-filtered DB count)", gotRemaining, tc.wantRemaining)
			}
		})
	}
}

// TestComputeViolationTally_NilResult verifies that a nil result does not panic
// and returns three zeros, logging an error.
func TestComputeViolationTally_NilResult(t *testing.T) {
	t.Parallel()

	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	gotFound, gotAutoFixed, gotRemaining := r.computeViolationTally(context.Background(), nil, "test-nil")

	if gotFound != 0 || gotAutoFixed != 0 || gotRemaining != 0 {
		t.Errorf("nil result: got (%d, %d, %d), want (0, 0, 0)", gotFound, gotAutoFixed, gotRemaining)
	}
	logged := buf.String()
	if !strings.Contains(logged, "computeViolationTally called with nil result") {
		t.Errorf("nil result: expected error log containing sentinel message, got %q", logged)
	}
}
