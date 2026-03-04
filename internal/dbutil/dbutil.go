package dbutil

import "time"

// BoolToInt converts a boolean to an integer (1 for true, 0 for false)
// for storage in SQLite, which has no native boolean type.
func BoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// IntToBool converts an integer to a boolean (non-zero is true).
func IntToBool(i int) bool {
	return i != 0
}

// ParseTime attempts to parse a time string using common database formats.
// It tries RFC3339, "2006-01-02 15:04:05", and "2006-01-02T15:04:05" in order,
// returning the zero time if none match.
func ParseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// FormatNullableTime formats a nullable time pointer as an RFC3339 string
// for SQL insertion. Returns nil (untyped) when t is nil so the database
// driver stores NULL.
func FormatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}

// NullableString returns nil (untyped) for an empty string so the database
// driver stores NULL, or returns the string itself otherwise.
func NullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NilableTime converts a nullable time pointer to a nullable RFC3339 string
// pointer, suitable for JSON serialization where omitempty applies.
func NilableTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
