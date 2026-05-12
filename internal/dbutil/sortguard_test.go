package dbutil

import "testing"

func TestValidateSortKey(t *testing.T) {
	allowed := map[string]string{
		"name":         "name",
		"sort_name":    "sort_name",
		"health_score": "health_score",
	}

	tests := []struct {
		name       string
		key        string
		wantColumn string
		wantOK     bool
	}{
		{
			name:       "empty key returns ok with empty column for caller default",
			key:        "",
			wantColumn: "",
			wantOK:     true,
		},
		{
			name:       "known key returns canonical column",
			key:        "name",
			wantColumn: "name",
			wantOK:     true,
		},
		{
			name:       "known key with different value returns mapped column",
			key:        "sort_name",
			wantColumn: "sort_name",
			wantOK:     true,
		},
		{
			name:       "unknown key rejected",
			key:        "evil",
			wantColumn: "",
			wantOK:     false,
		},
		{
			name:       "SQL injection attempt rejected",
			key:        "name; DROP TABLE artists",
			wantColumn: "",
			wantOK:     false,
		},
		{
			name:       "case-sensitive match",
			key:        "Name",
			wantColumn: "",
			wantOK:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			col, ok := ValidateSortKey(tc.key, allowed)
			if ok != tc.wantOK {
				t.Errorf("ValidateSortKey(%q) ok = %v, want %v", tc.key, ok, tc.wantOK)
			}
			if col != tc.wantColumn {
				t.Errorf("ValidateSortKey(%q) column = %q, want %q", tc.key, col, tc.wantColumn)
			}
		})
	}
}

func TestValidateSortKey_AllowlistWithRemappedColumn(t *testing.T) {
	// The allowlist maps the public key to a different SQL expression --
	// useful when the API key name differs from the column name (e.g. the
	// "severity" public key maps to a CASE expression).
	allowed := map[string]string{
		"severity": "CASE rv.severity WHEN 'error' THEN 3 ELSE 0 END",
	}
	col, ok := ValidateSortKey("severity", allowed)
	if !ok {
		t.Fatalf("expected ok=true for known key")
	}
	if col != "CASE rv.severity WHEN 'error' THEN 3 ELSE 0 END" {
		t.Errorf("expected mapped CASE expression, got %q", col)
	}
}

func TestValidateSortOrder(t *testing.T) {
	tests := []struct {
		input  string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"asc", "asc", true},
		{"desc", "desc", true},
		{"ASC", "", false},
		{"ascending", "", false},
		{"random", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := ValidateSortOrder(tc.input)
			if ok != tc.wantOK {
				t.Errorf("ValidateSortOrder(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("ValidateSortOrder(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
