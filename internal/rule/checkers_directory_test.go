package rule

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestCanonicalDirName_Prefix(t *testing.T) {
	tests := []struct {
		name        string
		articleMode string
		want        string
	}{
		{"The Beatles", "prefix", "The Beatles"},
		{"A Tribe Called Quest", "prefix", "A Tribe Called Quest"},
		{"An Albatross", "prefix", "An Albatross"},
		{"Radiohead", "prefix", "Radiohead"},
		{"The Beatles", "", "The Beatles"}, // default = prefix
	}
	for _, tt := range tests {
		got := canonicalDirName(tt.name, tt.articleMode)
		if got != tt.want {
			t.Errorf("canonicalDirName(%q, %q) = %q, want %q", tt.name, tt.articleMode, got, tt.want)
		}
	}
}

func TestCanonicalDirName_Suffix(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"The Beatles", "Beatles, The"},
		{"A Tribe Called Quest", "Tribe Called Quest, A"},
		{"An Albatross", "Albatross, An"},
		{"Radiohead", "Radiohead"},
		{"the rolling stones", "rolling stones, the"},
	}
	for _, tt := range tests {
		got := canonicalDirName(tt.name, "suffix")
		if got != tt.want {
			t.Errorf("canonicalDirName(%q, suffix) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCanonicalDirName_Strip(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"The Beatles", "Beatles"},
		{"A Tribe Called Quest", "Tribe Called Quest"},
		{"Radiohead", "Radiohead"},
	}
	for _, tt := range tests {
		got := canonicalDirName(tt.name, "strip")
		if got != tt.want {
			t.Errorf("canonicalDirName(%q, strip) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCanonicalDirName_InvalidChars(t *testing.T) {
	got := canonicalDirName("AC/DC", "prefix")
	if got != "AC_DC" {
		t.Errorf("canonicalDirName(AC/DC) = %q, want AC_DC", got)
	}
}

func TestCanonicalDirName_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{".", ""},
		{"..", ""},
	}
	for _, tt := range tests {
		got := canonicalDirName(tt.name, "prefix")
		if got != tt.want {
			t.Errorf("canonicalDirName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCheckDirectoryNameMismatch(t *testing.T) {
	cfg := RuleConfig{Severity: "warning", ArticleMode: "prefix"}

	t.Run("empty path", func(t *testing.T) {
		a := &artist.Artist{Name: "Test", Path: ""}
		if v := checkDirectoryNameMismatch(a, cfg); v != nil {
			t.Errorf("expected nil for empty path, got %+v", v)
		}
	})

	t.Run("matching", func(t *testing.T) {
		a := &artist.Artist{Name: "The Beatles", Path: "/music/The Beatles"}
		if v := checkDirectoryNameMismatch(a, cfg); v != nil {
			t.Errorf("expected nil for matching dir, got %+v", v)
		}
	})

	t.Run("case insensitive match", func(t *testing.T) {
		a := &artist.Artist{Name: "The Beatles", Path: "/music/the beatles"}
		if v := checkDirectoryNameMismatch(a, cfg); v != nil {
			t.Errorf("expected nil for case-insensitive match, got %+v", v)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		a := &artist.Artist{Name: "The Beatles", Path: "/music/Beatles"}
		v := checkDirectoryNameMismatch(a, cfg)
		if v == nil {
			t.Fatal("expected violation for mismatch, got nil")
		}
		if v.RuleID != RuleDirectoryNameMismatch {
			t.Errorf("RuleID = %q, want %q", v.RuleID, RuleDirectoryNameMismatch)
		}
		if !v.Fixable {
			t.Error("expected Fixable=true")
		}
	})

	t.Run("suffix mode match", func(t *testing.T) {
		cfgSuffix := RuleConfig{Severity: "warning", ArticleMode: "suffix"}
		a := &artist.Artist{Name: "The Beatles", Path: "/music/Beatles, The"}
		if v := checkDirectoryNameMismatch(a, cfgSuffix); v != nil {
			t.Errorf("expected nil for suffix mode match, got %+v", v)
		}
	})

	// NFC vs NFD: macOS filesystems store directory names in decomposed
	// (NFD) form, while provider metadata uses composed (NFC). A byte-level
	// comparison would flag a spurious mismatch; the checker must normalize
	// both sides before comparing.
	t.Run("nfc vs nfd equivalent no violation", func(t *testing.T) {
		nfd := "Maria Joa\u0303o Pires" // o + combining tilde
		nfc := "Maria Jo\u00e3o Pires"  // precomposed o-tilde
		a := &artist.Artist{Name: nfc, Path: "/music/" + nfd}
		if v := checkDirectoryNameMismatch(a, cfg); v != nil {
			t.Errorf("expected nil for NFC/NFD equivalent names, got %+v", v)
		}
	})

	// When directory names differ by both Unicode normalization form AND
	// letter case, the initial case-insensitive check fails on unnormalized
	// strings in different forms, and the normalized check must be
	// case-insensitive too to avoid a false-positive violation.
	t.Run("nfd lowercase vs nfc uppercase no violation", func(t *testing.T) {
		nfdLower := "maria joa\u0303o pires" // lowercase, decomposed
		nfcUpper := "Maria Jo\u00e3o Pires"  // mixed case, precomposed
		a := &artist.Artist{Name: nfcUpper, Path: "/music/" + nfdLower}
		if v := checkDirectoryNameMismatch(a, cfg); v != nil {
			t.Errorf("expected nil for NFC/NFD + case equivalent names, got %+v", v)
		}
	})
}
