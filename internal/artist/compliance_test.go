package artist

import (
	"encoding/json"
	"testing"
)

// TestComplianceStatus_ConstantValues pins the underlying string values of the
// ComplianceStatus constants. These values cross the API/UI boundary (they are
// serialized and compared as strings), so an accidental rename would silently
// break clients. This test fails loudly if any value changes.
func TestComplianceStatus_ConstantValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  ComplianceStatus
		want string
	}{
		{"compliant", ComplianceCompliant, "compliant"},
		{"warning", ComplianceWarning, "warning"},
		{"error", ComplianceError, "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if string(tt.got) != tt.want {
				t.Errorf("ComplianceStatus = %q, want %q", string(tt.got), tt.want)
			}
		})
	}
}

// TestComplianceStatus_Distinct guards against two constants accidentally
// collapsing to the same value (e.g. a copy-paste typo).
func TestComplianceStatus_Distinct(t *testing.T) {
	t.Parallel()

	all := []ComplianceStatus{ComplianceCompliant, ComplianceWarning, ComplianceError}
	seen := make(map[ComplianceStatus]bool, len(all))
	for _, s := range all {
		if seen[s] {
			t.Errorf("duplicate ComplianceStatus value %q", string(s))
		}
		seen[s] = true
	}
}

// TestComplianceStatus_JSON verifies ComplianceStatus serializes as its bare
// string value (it is a defined string type, so JSON is the quoted string).
func TestComplianceStatus_JSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ComplianceStatus
		want string
	}{
		{"compliant", ComplianceCompliant, `"compliant"`},
		{"warning", ComplianceWarning, `"warning"`},
		{"error", ComplianceError, `"error"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal(%q) returned error: %v", string(tt.in), err)
			}
			if string(got) != tt.want {
				t.Errorf("json.Marshal(%q) = %s, want %s", string(tt.in), got, tt.want)
			}

			// Round-trip back into a ComplianceStatus.
			var back ComplianceStatus
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", got, err)
			}
			if back != tt.in {
				t.Errorf("round-trip = %q, want %q", string(back), string(tt.in))
			}
		})
	}
}
