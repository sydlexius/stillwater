package artist

import "testing"

func TestIsIgnoredSystemName(t *testing.T) {
	t.Parallel()
	ignored := []string{
		"$RECYCLE.BIN", "$recycle.bin",
		"System Volume Information", "SYSTEM VOLUME INFORMATION",
		"@eaDir", "@EADIR",
		"@__thumb",
		".Trash", ".Trashes", ".DS_Store",
		"lost+found",
		"Thumbs.db", "thumbs.DB",
		"desktop.ini",
		"  @eaDir  ", // trimmed
	}
	for _, name := range ignored {
		if !IsIgnoredSystemName(name) {
			t.Errorf("IsIgnoredSystemName(%q) = false, want true", name)
		}
	}
	notIgnored := []string{"Nirvana", "The Beatles", "eaDir", "recycle", "systemvolume", "found", ""}
	for _, name := range notIgnored {
		if IsIgnoredSystemName(name) {
			t.Errorf("IsIgnoredSystemName(%q) = true, want false", name)
		}
	}
}

func TestIsNonArtistDirName(t *testing.T) {
	t.Parallel()
	excluded := []string{
		"Various Artists", "various artists", "VARIOUS ARTISTS",
		"Various", "various",
		"VA", "va",
		"Various Artist",
		"  VA  ", // trimmed
	}
	for _, name := range excluded {
		if !IsNonArtistDirName(name) {
			t.Errorf("IsNonArtistDirName(%q) = false, want true", name)
		}
	}
	// Over-broad-match guard: real artists that merely contain the tokens.
	kept := []string{"Various Voices", "Vanessa", "Vanguard", "The Various", "Vast", ""}
	for _, name := range kept {
		if IsNonArtistDirName(name) {
			t.Errorf("IsNonArtistDirName(%q) = true, want false", name)
		}
	}
}

func TestIsAdditiveMergeDir(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"extrafanart", "ExtraFanart", "EXTRAFANART", "extrathumbs", "  extrafanart  "} {
		if !isAdditiveMergeDir(name) {
			t.Errorf("isAdditiveMergeDir(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"fanart", "extras", "extrafanarts", "thumbs", ""} {
		if isAdditiveMergeDir(name) {
			t.Errorf("isAdditiveMergeDir(%q) = true, want false", name)
		}
	}
}
