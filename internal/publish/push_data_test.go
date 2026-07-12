package publish

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestDeriveSortNameFallback enumerates the (name, mbSort) inputs the
// numeric-prefix derivation rule (#1083) must handle. Cases pin both the
// returned sortName and the derived flag (which gates whether the push
// code adds "SortName" to LockedFields on Emby; Jellyfin intentionally
// ignores LockSortName).
func TestDeriveSortNameFallback(t *testing.T) {
	cases := []struct {
		name       string
		mbSort     string
		wantSort   string
		wantLocked bool
		desc       string
	}{
		{
			name: "Radiohead", mbSort: "Radiohead",
			wantSort: "Radiohead", wantLocked: false,
			desc: "MB-provided sort survives; never locked",
		},
		{
			name: "The Cure", mbSort: "Cure, The",
			wantSort: "Cure, The", wantLocked: false,
			desc: "MB-provided 'Cure, The' wins over name-prefix derivation",
		},
		{
			name: "12 Pebbles", mbSort: "",
			wantSort: "0000000012 Pebbles", wantLocked: true,
			desc: "two-digit prefix zero-padded to 10",
		},
		{
			name: "3 Doors Down", mbSort: "",
			wantSort: "0000000003 Doors Down", wantLocked: true,
			desc: "single-digit prefix zero-padded to 10",
		},
		{
			name: "311", mbSort: "",
			wantSort: "0000000311", wantLocked: true,
			desc: "all-numeric name padded with no trailing remainder",
		},
		{
			name: "38 Special", mbSort: "",
			wantSort: "0000000038 Special", wantLocked: true,
			desc: "two-digit prefix with single trailing word",
		},
		{
			name: "1349", mbSort: "",
			wantSort: "0000001349", wantLocked: true,
			desc: "four-digit all-numeric (norwegian black metal) padded to 10",
		},
		{
			name: "", mbSort: "",
			wantSort: "", wantLocked: false,
			desc: "empty name + empty mbSort returns empty no-op",
		},
		{
			name: "Bjork", mbSort: "",
			wantSort: "", wantLocked: false,
			desc: "alphabetic prefix without MB sort: pass through empty",
		},
		{
			name: "!!!", mbSort: "",
			wantSort: "", wantLocked: false,
			desc: "leading-symbol name out of scope; passes through empty",
		},
		{
			name: "10000 Maniacs", mbSort: "",
			wantSort: "0000010000 Maniacs", wantLocked: true,
			desc: "five-digit prefix padded to width",
		},
		{
			name: "12345678901 Maniacs", mbSort: "",
			wantSort: "12345678901 Maniacs", wantLocked: true,
			desc: "prefix already wider than pad width: no padding, still locked",
		},
		{
			// Arabic-Indic digit is intentionally out of scope; pad-only-on-ASCII
			// guards keep the platform-side sort behavior predictable.
			name: "٠ Test", mbSort: "",
			wantSort: "", wantLocked: false,
			desc: "Unicode digit prefix is out of scope; passes through empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			gotSort, gotLocked := deriveSortNameFallback(tc.name, tc.mbSort)
			if gotSort != tc.wantSort {
				t.Errorf("sortName = %q, want %q", gotSort, tc.wantSort)
			}
			if gotLocked != tc.wantLocked {
				t.Errorf("locked = %v, want %v", gotLocked, tc.wantLocked)
			}
		})
	}
}

// TestBuildArtistPushData_SortNameDerivation_Propagation pins that the
// derivation flag set by deriveSortNameFallback survives into the
// ArtistPushData LockSortName field that the per-platform push code
// reads. A regression that drops the field on the floor would silently
// leave numeric-prefix artists un-locked on the platform side and the
// next metadata refresh would clear the derived value (#1083).
func TestBuildArtistPushData_SortNameDerivation_Propagation(t *testing.T) {
	t.Run("numeric prefix + empty SortName: derived + locked", func(t *testing.T) {
		a := &artist.Artist{Name: "12 Pebbles", SortName: "", Type: "group"}
		got := BuildArtistPushData(a, nil)
		if got.SortName != "0000000012 Pebbles" {
			t.Errorf("SortName = %q, want %q", got.SortName, "0000000012 Pebbles")
		}
		if !got.LockSortName {
			t.Errorf("LockSortName = false, want true for derived numeric prefix")
		}
	})

	t.Run("MB-provided SortName: pass-through, not locked", func(t *testing.T) {
		a := &artist.Artist{Name: "Radiohead", SortName: "Radiohead", Type: "group"}
		got := BuildArtistPushData(a, nil)
		if got.SortName != "Radiohead" {
			t.Errorf("SortName = %q, want %q", got.SortName, "Radiohead")
		}
		if got.LockSortName {
			t.Errorf("LockSortName = true, want false when MB provided SortName")
		}
	})

	t.Run("non-numeric name + empty MB sort: empty pass-through, not locked", func(t *testing.T) {
		a := &artist.Artist{Name: "Bjork", SortName: "", Type: "solo"}
		got := BuildArtistPushData(a, nil)
		if got.SortName != "" {
			t.Errorf("SortName = %q, want empty (non-numeric prefix is out of scope)", got.SortName)
		}
		if got.LockSortName {
			t.Errorf("LockSortName = true, want false")
		}
	})

	t.Run("MB SortName wins even on numeric-prefix name", func(t *testing.T) {
		// User has explicitly set Cure-The-style sort name on a numeric artist;
		// derivation must NOT override.
		a := &artist.Artist{Name: "12 Pebbles", SortName: "Pebbles, 12", Type: "group"}
		got := BuildArtistPushData(a, nil)
		if got.SortName != "Pebbles, 12" {
			t.Errorf("SortName = %q, want %q (MB/user wins)", got.SortName, "Pebbles, 12")
		}
		if got.LockSortName {
			t.Errorf("LockSortName = true, want false when user/MB SortName wins")
		}
	})
}
