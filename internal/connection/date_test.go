package connection

import "testing"

func TestNormalizeDateForPlatform(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"year only", "1991", "1991-01-01"},
		{"year-month", "1991-05", "1991-05-01"},
		{"full date", "1991-05-27", "1991-05-27"},
		{"ISO 8601 full precision", "1985-01-01T00:00:00.0000000Z", "1985-01-01T00:00:00.0000000Z"},
		{"ISO 8601 short", "2006-06-15T00:00:00Z", "2006-06-15T00:00:00Z"},
		{"named month day year", "October 14, 1946", "1946-10-14"},
		{"named month with location", "October 14, 1946 in Abingdon, England", "1946-10-14"},
		{"year with location", "2006 in Cardiff, CA", "2006-01-01"},
		{"month year", "January 2006", "2006-01-01"},
		{"unparseable", "not a date", ""},
		{"year in text", "some text 1987 more text", "1987-01-01"},
		{"whitespace padded", "   1991   ", "1991-01-01"},
		{"short month name", "Oct 14, 1946", "1946-10-14"},
		{"day month year", "14 October 1946", "1946-10-14"},
		{"short month year", "Jan 2006", "2006-01-01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeDateForPlatform(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeDateForPlatform(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
