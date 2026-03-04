package dbutil

import (
	"testing"
	"time"
)

func TestBoolToInt(t *testing.T) {
	tests := []struct {
		name string
		in   bool
		want int
	}{
		{"true", true, 1},
		{"false", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BoolToInt(tt.in); got != tt.want {
				t.Errorf("BoolToInt(%v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestIntToBool(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want bool
	}{
		{"zero", 0, false},
		{"one", 1, true},
		{"negative", -1, true},
		{"large", 42, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IntToBool(tt.in); got != tt.want {
				t.Errorf("IntToBool(%d) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseTime(t *testing.T) {
	ref := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{"RFC3339", "2024-06-15T10:30:00Z", ref},
		{"space-separated", "2024-06-15 10:30:00", ref},
		{"T-separated no zone", "2024-06-15T10:30:00", ref},
		{"empty string", "", time.Time{}},
		{"garbage", "not-a-time", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTime(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("ParseTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatNullableTime(t *testing.T) {
	ref := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	t.Run("nil", func(t *testing.T) {
		if got := FormatNullableTime(nil); got != nil {
			t.Errorf("FormatNullableTime(nil) = %v, want nil", got)
		}
	})

	t.Run("non-nil", func(t *testing.T) {
		got := FormatNullableTime(&ref)
		s, ok := got.(string)
		if !ok {
			t.Fatalf("FormatNullableTime returned %T, want string", got)
		}
		if s != "2024-06-15T10:30:00Z" {
			t.Errorf("FormatNullableTime = %q, want %q", s, "2024-06-15T10:30:00Z")
		}
	})
}

func TestNullableString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
	}{
		{"empty", "", true},
		{"non-empty", "hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NullableString(tt.in)
			if tt.wantNil {
				if got != nil {
					t.Errorf("NullableString(%q) = %v, want nil", tt.in, got)
				}
			} else {
				s, ok := got.(string)
				if !ok || s != tt.in {
					t.Errorf("NullableString(%q) = %v, want %q", tt.in, got, tt.in)
				}
			}
		})
	}
}

func TestNilableTime(t *testing.T) {
	ref := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	t.Run("nil", func(t *testing.T) {
		if got := NilableTime(nil); got != nil {
			t.Errorf("NilableTime(nil) = %v, want nil", got)
		}
	})

	t.Run("non-nil", func(t *testing.T) {
		got := NilableTime(&ref)
		if got == nil {
			t.Fatal("NilableTime returned nil, want non-nil")
		}
		if *got != "2024-06-15T10:30:00Z" {
			t.Errorf("NilableTime = %q, want %q", *got, "2024-06-15T10:30:00Z")
		}
	})
}
