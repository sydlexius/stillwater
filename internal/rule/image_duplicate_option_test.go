package rule

import (
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// TestPlatformPruneThreshold pins what the Settings screen is told about the
// duplicate-images rule's stored tolerance. The boundary rows (0.85 accepted,
// 0.84 refused; 1.0 accepted, 1.01 refused) are the ones that matter: loosening
// either comparison in platformPruneTolerance flips exactly one of them.
func TestPlatformPruneThreshold(t *testing.T) {
	tests := []struct {
		name          string
		configured    float64
		wantThreshold float64
		wantAccepted  bool
	}{
		{"unset reads as the rule default", 0, img.DefaultDuplicateTolerance, true},
		{"the floor itself is accepted", 0.85, 0.85, true},
		{"the ceiling itself is accepted", 1.0, 1.0, true},
		{"a value inside the range is accepted", 0.95, 0.95, true},
		{"just below the floor is refused", 0.84, 0.84, false},
		{"just above the ceiling is refused", 1.01, 1.01, false},
		{"a fat-fingered loose value is refused", 0.5, 0.5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotThreshold, gotAccepted := PlatformPruneThreshold(tt.configured)
			if gotThreshold != tt.wantThreshold {
				t.Errorf("PlatformPruneThreshold(%v) threshold = %v, want %v", tt.configured, gotThreshold, tt.wantThreshold)
			}
			if gotAccepted != tt.wantAccepted {
				t.Errorf("PlatformPruneThreshold(%v) accepted = %v, want %v", tt.configured, gotAccepted, tt.wantAccepted)
			}
		})
	}

	// The screen and the fixer must agree for every value, not just the rows
	// above: this catches a wrapper that starts re-implementing the range.
	for i := 0; i <= 120; i++ {
		v := float64(i) / 100
		wantThreshold, wantAccepted := platformPruneTolerance(v)
		gotThreshold, gotAccepted := PlatformPruneThreshold(v)
		if gotThreshold != wantThreshold || gotAccepted != wantAccepted {
			t.Errorf("PlatformPruneThreshold(%v) = (%v, %v); the fixer's platformPruneTolerance gives (%v, %v)",
				v, gotThreshold, gotAccepted, wantThreshold, wantAccepted)
		}
	}
}
