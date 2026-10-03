package rule

import (
	"context"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// The sweep's policy is the fixer's: on only when the rule is enabled AND the
// option is set AND platformPruneTolerance accepts the tolerance, and at the
// tolerance that function returns. Read from the stored rule on every call, so
// a change needs no restart.
func TestPlatformDupSweepPolicy(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	set := func(enabled, prune bool, tol float64) {
		t.Helper()
		r, err := svc.GetByID(ctx, RuleImageDuplicate)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		r.Enabled, r.Config.PrunePlatformCopies, r.Config.Tolerance = enabled, prune, tol
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	// The seeded default: the option is off, so the sweep is off.
	if _, on, err := svc.PlatformDupSweepPolicy(ctx); err != nil || on {
		t.Fatalf("seeded default: on=%v err=%v, want off", on, err)
	}
	for _, tc := range []struct {
		name           string
		enabled, prune bool
		configured     float64
		wantOn         bool
		wantTol        float64
	}{
		{"option on, explicit tolerance", true, true, 0.95, true, 0.95},
		{"option on, unset tolerance is the rule default", true, true, 0, true, img.DefaultDuplicateTolerance},
		{"tolerance below the platform floor is refused", true, true, 0.5, false, 0},
		{"tolerance above 1 is refused", true, true, 1.5, false, 0},
		{"rule disabled", false, true, 0.95, false, 0},
		{"option off", true, false, 0.95, false, 0},
	} {
		set(tc.enabled, tc.prune, tc.configured)
		tol, on, err := svc.PlatformDupSweepPolicy(ctx)
		if err != nil || on != tc.wantOn || tol != tc.wantTol { // 0 on every disabled path
			t.Errorf("%s: tolerance=%v on=%v err=%v, want %v/%v", tc.name, tol, on, err, tc.wantTol, tc.wantOn)
		}
		// Agreement with the fixer, by construction.
		if fixTol, fixOK := platformPruneTolerance(tc.configured); on && (!fixOK || fixTol != tol) {
			t.Errorf("%s: sweep tolerance %v disagrees with the fixer's %v (ok=%v)", tc.name, tol, fixTol, fixOK)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, RuleImageDuplicate); err != nil {
		t.Fatal(err)
	}
	if _, on, err := svc.PlatformDupSweepPolicy(ctx); err != nil || on {
		t.Errorf("rule row absent: on=%v err=%v, want off without an error", on, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, on, err := svc.PlatformDupSweepPolicy(ctx); err == nil || on {
		t.Errorf("unreadable rule: on=%v err=%v, want an error and off", on, err)
	}
}
