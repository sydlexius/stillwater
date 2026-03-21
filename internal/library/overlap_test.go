package library

import (
	"testing"
)

func TestPathsOverlap(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "identical paths",
			a:    "/music",
			b:    "/music",
			want: true,
		},
		{
			name: "a is prefix of b",
			a:    "/music",
			b:    "/music/rock",
			want: true,
		},
		{
			name: "b is prefix of a",
			a:    "/music/rock",
			b:    "/music",
			want: true,
		},
		{
			name: "no overlap",
			a:    "/music",
			b:    "/videos",
			want: false,
		},
		{
			name: "partial name match but not prefix",
			a:    "/music",
			b:    "/music2",
			want: false,
		},
		{
			name: "deeper nesting",
			a:    "/data/media/music",
			b:    "/data/media/music/artists/jazz",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathsOverlap(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("pathsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDetectOverlaps(t *testing.T) {
	t.Run("no overlap between unrelated libraries", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "Manual Music", Path: tmp + "/music", Source: SourceManual},
			{ID: "2", Name: "Emby Movies", Path: tmp + "/movies", Source: SourceEmby},
		}
		results := DetectOverlaps(libs)
		if len(results) != 0 {
			t.Errorf("expected 0 overlaps, got %d", len(results))
		}
	})

	t.Run("overlap between manual and emby at same path", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "My Music", Path: tmp + "/music", Source: SourceManual},
			{ID: "2", Name: "Emby Music", Path: tmp + "/music", Source: SourceEmby},
		}
		results := DetectOverlaps(libs)
		if len(results) < 2 {
			t.Fatalf("expected at least 2 overlaps (both libraries flagged), got %d", len(results))
		}
		// Both libraries should be flagged
		found := make(map[string]bool)
		for _, r := range results {
			found[r.LibraryID] = true
		}
		if !found["1"] {
			t.Error("expected manual library to be flagged")
		}
		if !found["2"] {
			t.Error("expected emby library to be flagged")
		}
	})

	t.Run("overlap between emby and jellyfin at same path", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "Emby Music", Path: tmp + "/music", Source: SourceEmby},
			{ID: "2", Name: "Jellyfin Music", Path: tmp + "/music", Source: SourceJellyfin},
		}
		results := DetectOverlaps(libs)
		if len(results) < 2 {
			t.Fatalf("expected at least 2 overlaps, got %d", len(results))
		}
	})

	t.Run("prefix overlap detected", func(t *testing.T) {
		// Use temp dirs to avoid symlink resolution issues with system paths.
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "My Music", Path: tmp + "/media/music", Source: SourceManual},
			{ID: "2", Name: "Emby All", Path: tmp + "/media", Source: SourceEmby},
		}
		results := DetectOverlaps(libs)
		if len(results) == 0 {
			t.Error("expected overlap to be detected for prefix paths")
		}
	})

	t.Run("pathless libraries are skipped", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "Emby Music", Path: "", Source: SourceEmby},
			{ID: "2", Name: "Manual Music", Path: tmp + "/music", Source: SourceManual},
		}
		results := DetectOverlaps(libs)
		if len(results) != 0 {
			t.Errorf("expected 0 overlaps for pathless library, got %d", len(results))
		}
	})

	t.Run("two manual libraries do not trigger overlap", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "Music A", Path: tmp + "/music", Source: SourceManual},
			{ID: "2", Name: "Music B", Path: tmp + "/music", Source: SourceManual},
		}
		results := DetectOverlaps(libs)
		if len(results) != 0 {
			t.Errorf("expected 0 overlaps for two manual libraries, got %d", len(results))
		}
	})

	t.Run("lidarr source is not flagged as platform", func(t *testing.T) {
		tmp := t.TempDir()
		libs := []Library{
			{ID: "1", Name: "Lidarr Music", Path: tmp + "/music", Source: SourceLidarr},
			{ID: "2", Name: "Manual Music", Path: tmp + "/music", Source: SourceManual},
		}
		results := DetectOverlaps(libs)
		if len(results) != 0 {
			t.Errorf("expected 0 overlaps for lidarr+manual, got %d", len(results))
		}
	})
}

func TestCleanPathEdgeCases(t *testing.T) {
	// Use a temp directory as the base to avoid interference from real
	// symlinks on the host filesystem (e.g. /music -> /mnt/d/.../music).
	tmp := t.TempDir()

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "mixed case paths treated as equal",
			a:    tmp + "/Music/Rock",
			b:    tmp + "/music/rock",
			want: true,
		},
		{
			name: "mixed case prefix overlap",
			a:    tmp + "/Music",
			b:    tmp + "/music/Rock",
			want: true,
		},
		{
			name: "trailing slash stripped",
			a:    tmp + "/music/",
			b:    tmp + "/music",
			want: true,
		},
		{
			name: "both have trailing slashes",
			a:    tmp + "/music/rock/",
			b:    tmp + "/music/",
			want: true,
		},
		{
			name: "trailing slash does not create false overlap",
			a:    tmp + "/music/",
			b:    tmp + "/videos/",
			want: false,
		},
		{
			name: "double slashes cleaned",
			a:    tmp + "/music//rock",
			b:    tmp + "/music/rock",
			want: true,
		},
		{
			name: "dot segments cleaned",
			a:    tmp + "/music/./rock",
			b:    tmp + "/music/rock",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use cleanPath to normalize like DetectOverlaps does, then
			// check pathsOverlap on the cleaned results.
			ca := cleanPath(tt.a)
			cb := cleanPath(tt.b)
			got := pathsOverlap(ca, cb)
			if got != tt.want {
				t.Errorf("pathsOverlap(cleanPath(%q), cleanPath(%q)) = %v, want %v (cleaned: %q, %q)",
					tt.a, tt.b, got, tt.want, ca, cb)
			}
		})
	}
}

func TestIsPlatformSource(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{SourceEmby, true},
		{SourceJellyfin, true},
		{SourceLidarr, false},
		{SourceManual, false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			if got := isPlatformSource(tt.source); got != tt.want {
				t.Errorf("isPlatformSource(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}
