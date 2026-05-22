package library

import "testing"

func TestLibrary_FSWatchEnabled(t *testing.T) {
	cases := []struct {
		name    string
		fsWatch int
		want    bool
	}{
		{"off", 0, false},
		{"watch only", FSModeWatch, true},
		{"poll only", FSModePoll, false},
		{"watch and poll", FSModeWatch | FSModePoll, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Library{FSWatch: tc.fsWatch}).FSWatchEnabled(); got != tc.want {
				t.Errorf("FSWatchEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLibrary_FSPollEnabled(t *testing.T) {
	cases := []struct {
		name    string
		fsWatch int
		want    bool
	}{
		{"off", 0, false},
		{"watch only", FSModeWatch, false},
		{"poll only", FSModePoll, true},
		{"watch and poll", FSModeWatch | FSModePoll, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Library{FSWatch: tc.fsWatch}).FSPollEnabled(); got != tc.want {
				t.Errorf("FSPollEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLibrary_IsClassical(t *testing.T) {
	if !(Library{Type: TypeClassical}).IsClassical() {
		t.Error("IsClassical() = false for a classical library, want true")
	}
	if (Library{Type: TypeRegular}).IsClassical() {
		t.Error("IsClassical() = true for a regular library, want false")
	}
}

func TestLibrary_SourceDisplayName(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{"emby", SourceEmby, "Emby"},
		{"jellyfin", SourceJellyfin, "Jellyfin"},
		{"lidarr", SourceLidarr, "Lidarr"},
		{"manual", SourceManual, ""},
		{"unknown", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Library{Source: tc.source}).SourceDisplayName(); got != tc.want {
				t.Errorf("SourceDisplayName() for source %q = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestIsValidPollInterval(t *testing.T) {
	for _, v := range ValidPollIntervals {
		if !IsValidPollInterval(v) {
			t.Errorf("IsValidPollInterval(%d) = false, want true for an allowed interval", v)
		}
	}
	for _, v := range []int{0, 1, 120, -60, 3600} {
		if IsValidPollInterval(v) {
			t.Errorf("IsValidPollInterval(%d) = true, want false for a disallowed interval", v)
		}
	}
}
