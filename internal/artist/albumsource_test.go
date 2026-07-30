package artist

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestAlbumEvidenceZeroValueIsUnknown is the guard on the load-bearing design
// decision: the zero value of AlbumEvidence must be EvidenceUnknown, so that a
// caller which forgets to populate Evidence cannot accidentally assert "this
// artist has no albums". Asserting the const directly AND asserting it through
// an uninitialized struct field covers both the enum ordering and the field.
func TestAlbumEvidenceZeroValueIsUnknown(t *testing.T) {
	var zero AlbumEvidence
	if zero != EvidenceUnknown {
		t.Errorf("zero AlbumEvidence = %v, want EvidenceUnknown: a forgotten field must not assert 'no albums'", zero)
	}

	var set AlbumSet
	if set.Evidence != EvidenceUnknown {
		t.Errorf("zero AlbumSet.Evidence = %v, want EvidenceUnknown", set.Evidence)
	}

	// Precondition on the enum itself: if EvidenceNone were 0 the assertions
	// above would still pass under a mutation that renamed the constants, so
	// pin the numeric ordering too.
	if int(EvidenceUnknown) != 0 {
		t.Errorf("EvidenceUnknown = %d, want 0", int(EvidenceUnknown))
	}
	if EvidenceNone == EvidenceUnknown || EvidenceFound == EvidenceUnknown || EvidenceNone == EvidenceFound {
		t.Error("the three evidence states must be distinct")
	}
}

func TestAlbumEvidenceString(t *testing.T) {
	for _, tc := range []struct {
		ev   AlbumEvidence
		want string
	}{
		{EvidenceUnknown, "unknown"},
		{EvidenceNone, "none"},
		{EvidenceFound, "found"},
		{AlbumEvidence(99), "AlbumEvidence(99)"},
	} {
		if got := tc.ev.String(); got != tc.want {
			t.Errorf("AlbumEvidence(%d).String() = %q, want %q", int(tc.ev), got, tc.want)
		}
	}
}

// TestFilesystemAlbumSource_ThreeStates proves the three-way distinction that
// ListLocalAlbums cannot make. All three cases collapse to a nil slice through
// the old function, so each state is asserted independently here.
func TestFilesystemAlbumSource_ThreeStates(t *testing.T) {
	src := NewFilesystemAlbumSource()
	ctx := context.Background()

	t.Run("no path recorded is Unknown", func(t *testing.T) {
		a := &Artist{Name: "Pathless"}
		// Precondition: the case under test is genuinely "no path", not an
		// empty directory that happens to exist.
		if a.Path != "" {
			t.Fatalf("precondition: Path = %q, want empty", a.Path)
		}

		set, err := src.LocalAlbums(ctx, a)
		if set.Evidence != EvidenceUnknown {
			t.Errorf("Evidence = %v, want EvidenceUnknown: nobody looked, so nothing was determined", set.Evidence)
		}
		if err == nil {
			t.Error("want a diagnostic error explaining the artist has no path")
		}
		if len(set.Titles) != 0 {
			t.Errorf("Titles = %v, want empty", set.Titles)
		}
	})

	t.Run("readable but empty directory is None", func(t *testing.T) {
		dir := t.TempDir()
		// Precondition: the directory must be readable and genuinely empty, or
		// this case is indistinguishable from the unreadable one below.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("precondition: temp dir not readable: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("precondition: temp dir has %d entries, want 0", len(entries))
		}

		set, err := src.LocalAlbums(ctx, &Artist{Name: "Empty", Path: dir})
		if err != nil {
			t.Errorf("unexpected error on a successful read: %v", err)
		}
		if set.Evidence != EvidenceNone {
			t.Errorf("Evidence = %v, want EvidenceNone: the read succeeded and found nothing", set.Evidence)
		}
	})

	t.Run("hidden-only directory is None", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		set, err := src.LocalAlbums(ctx, &Artist{Name: "HiddenOnly", Path: dir})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if set.Evidence != EvidenceNone {
			t.Errorf("Evidence = %v, want EvidenceNone: hidden dirs are not albums but the read succeeded", set.Evidence)
		}
	})

	t.Run("album subdirectories are Found", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"OK Computer", "Kid A", ".hidden"} {
			if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
		// A plain file must not count as an album.
		if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}

		set, err := src.LocalAlbums(ctx, &Artist{Name: "Radiohead", Path: dir})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if set.Evidence != EvidenceFound {
			t.Fatalf("Evidence = %v, want EvidenceFound", set.Evidence)
		}
		want := []string{"Kid A", "OK Computer"} // sorted, hidden and files excluded
		if len(set.Titles) != len(want) {
			t.Fatalf("Titles = %v, want %v", set.Titles, want)
		}
		for i := range want {
			if set.Titles[i] != want[i] {
				t.Errorf("Titles[%d] = %q, want %q", i, set.Titles[i], want[i])
			}
		}
		if set.Origin != "filesystem" {
			t.Errorf("Origin = %q, want %q", set.Origin, "filesystem")
		}
	})

	t.Run("unreadable directory is Unknown, never None", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permission bits do not restrict reads")
		}
		parent := t.TempDir()
		dir := filepath.Join(parent, "artist")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		// Put a real album inside FIRST, so a passing test cannot be explained
		// by the directory being empty: the only reason the source can fail to
		// see this album is the read error.
		if err := os.Mkdir(filepath.Join(dir, "Kid A"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatalf("setup: chmod 000: %v", err)
		}
		t.Cleanup(func() {
			// Restore so t.TempDir's own cleanup can remove the tree.
			_ = os.Chmod(dir, 0o755)
		})

		// Precondition: confirm the OS actually denies the read. Without this
		// the test passes vacuously on any platform or filesystem that ignores
		// the mode bits.
		if _, err := os.ReadDir(dir); err == nil {
			t.Skip("filesystem does not enforce directory mode bits; cannot exercise the read-error path")
		}

		set, err := src.LocalAlbums(ctx, &Artist{Name: "Unreadable", Path: dir})
		if set.Evidence == EvidenceNone {
			t.Fatal("Evidence = EvidenceNone on an unreadable directory: an unmounted or unreadable path must NEVER read as 'this artist has no albums'")
		}
		if set.Evidence != EvidenceUnknown {
			t.Errorf("Evidence = %v, want EvidenceUnknown", set.Evidence)
		}
		if err == nil {
			t.Error("want a diagnostic error naming the read failure")
		}
	})

	t.Run("canceled context is Unknown", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "Kid A"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		set, err := src.LocalAlbums(canceled, &Artist{Name: "Radiohead", Path: dir})
		if set.Evidence != EvidenceUnknown {
			t.Errorf("Evidence = %v, want EvidenceUnknown on a canceled context", set.Evidence)
		}
		if err == nil {
			t.Error("want an error reporting the cancellation")
		}
	})
}

// stubAlbumSource is a fixed answer for chain tests.
type stubAlbumSource struct {
	name string
	set  AlbumSet
	err  error
}

func (s stubAlbumSource) Name() string { return s.name }

func (s stubAlbumSource) LocalAlbums(context.Context, *Artist) (AlbumSet, error) {
	return s.set, s.err
}

func unknownSource(name string) stubAlbumSource {
	return stubAlbumSource{name: name, set: AlbumSet{Evidence: EvidenceUnknown, Origin: name}, err: os.ErrPermission}
}

func noneSource(name string) stubAlbumSource {
	return stubAlbumSource{name: name, set: AlbumSet{Evidence: EvidenceNone, Origin: name}}
}

func foundSource(name string, titles ...string) stubAlbumSource {
	return stubAlbumSource{name: name, set: AlbumSet{Evidence: EvidenceFound, Titles: titles, Origin: name}}
}

// TestChainAlbumSource_Ordering covers every combination that distinguishes the
// chain rule, with each source's answer and identity differing so no case can
// pass by coincidence. The Unknown-then-None case is the load-bearing one: one
// unreachable source must never be able to produce "this artist has no albums".
func TestChainAlbumSource_Ordering(t *testing.T) {
	tests := []struct {
		name         string
		sources      []AlbumSource
		wantEvidence AlbumEvidence
		wantTitles   []string
		wantOrigin   string
	}{
		{
			name:         "unknown then found: Found wins",
			sources:      []AlbumSource{unknownSource("a"), foundSource("b", "Kid A")},
			wantEvidence: EvidenceFound,
			wantTitles:   []string{"Kid A"},
			wantOrigin:   "b",
		},
		{
			name:         "none then none: unanimous None",
			sources:      []AlbumSource{noneSource("a"), noneSource("b")},
			wantEvidence: EvidenceNone,
			wantOrigin:   "chain",
		},
		{
			name:         "unknown then none: Unknown, NOT None",
			sources:      []AlbumSource{unknownSource("a"), noneSource("b")},
			wantEvidence: EvidenceUnknown,
			wantOrigin:   "chain",
		},
		{
			name:         "none then unknown: Unknown, NOT None (order-independent)",
			sources:      []AlbumSource{noneSource("a"), unknownSource("b")},
			wantEvidence: EvidenceUnknown,
			wantOrigin:   "chain",
		},
		{
			name:         "found then found: first wins",
			sources:      []AlbumSource{foundSource("a", "OK Computer"), foundSource("b", "Amnesiac")},
			wantEvidence: EvidenceFound,
			wantTitles:   []string{"OK Computer"},
			wantOrigin:   "a",
		},
		{
			name:         "none then found: Found wins over a determined-empty source",
			sources:      []AlbumSource{noneSource("a"), foundSource("b", "Kid A")},
			wantEvidence: EvidenceFound,
			wantTitles:   []string{"Kid A"},
			wantOrigin:   "b",
		},
		{
			name:         "unknown then unknown: Unknown",
			sources:      []AlbumSource{unknownSource("a"), unknownSource("b")},
			wantEvidence: EvidenceUnknown,
			wantOrigin:   "chain",
		},
		{
			name:         "empty chain: Unknown, never a vacuous None",
			sources:      nil,
			wantEvidence: EvidenceUnknown,
			wantOrigin:   "chain",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := NewChainAlbumSource(tc.sources...)
			set, _ := chain.LocalAlbums(context.Background(), &Artist{Name: "Radiohead"})

			if set.Evidence != tc.wantEvidence {
				t.Errorf("Evidence = %v, want %v", set.Evidence, tc.wantEvidence)
			}
			if len(set.Titles) != len(tc.wantTitles) {
				t.Fatalf("Titles = %v, want %v", set.Titles, tc.wantTitles)
			}
			for i := range tc.wantTitles {
				if set.Titles[i] != tc.wantTitles[i] {
					t.Errorf("Titles[%d] = %q, want %q", i, set.Titles[i], tc.wantTitles[i])
				}
			}
			if set.Origin != tc.wantOrigin {
				t.Errorf("Origin = %q, want %q", set.Origin, tc.wantOrigin)
			}
		})
	}
}

// TestChainAlbumSource_ShortCircuitsOnFound proves later sources are not
// consulted once one finds albums. A counter is the only way to see this; the
// returned set alone cannot distinguish "we stopped" from "we looked and
// ignored it".
func TestChainAlbumSource_ShortCircuitsOnFound(t *testing.T) {
	var laterCalls int
	counting := countingSource{name: "later", calls: &laterCalls}

	chain := NewChainAlbumSource(foundSource("first", "Kid A"), counting)
	set, err := chain.LocalAlbums(context.Background(), &Artist{Name: "Radiohead"})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if set.Evidence != EvidenceFound {
		t.Fatalf("Evidence = %v, want EvidenceFound", set.Evidence)
	}
	if laterCalls != 0 {
		t.Errorf("later source consulted %d times, want 0: Found must short-circuit", laterCalls)
	}
}

type countingSource struct {
	name  string
	calls *int
}

func (s countingSource) Name() string { return s.name }

func (s countingSource) LocalAlbums(context.Context, *Artist) (AlbumSet, error) {
	*s.calls++
	return AlbumSet{Evidence: EvidenceNone, Origin: s.name}, nil
}

// TestChainAlbumSource_ReportsSourceErrors checks the diagnostic error survives
// even though Evidence is what policy reads.
func TestChainAlbumSource_ReportsSourceErrors(t *testing.T) {
	chain := NewChainAlbumSource(unknownSource("a"), unknownSource("b"))
	_, err := chain.LocalAlbums(context.Background(), &Artist{Name: "Radiohead"})
	if err == nil {
		t.Fatal("want the per-source failures reported for diagnostics")
	}

	// A chain where nothing failed must report no error, or callers cannot tell
	// a real failure from routine operation.
	chain = NewChainAlbumSource(noneSource("a"), noneSource("b"))
	if _, err := chain.LocalAlbums(context.Background(), &Artist{Name: "Radiohead"}); err != nil {
		t.Errorf("unexpected error when every source succeeded: %v", err)
	}
}

// TestChainAlbumSource_TrustsEvidenceOverError covers a source that reports a
// partial failure alongside a usable determination. Evidence is the contract,
// so the chain must accept the Found answer.
func TestChainAlbumSource_TrustsEvidenceOverError(t *testing.T) {
	partial := stubAlbumSource{
		name: "partial",
		set:  AlbumSet{Evidence: EvidenceFound, Titles: []string{"Kid A"}, Origin: "partial"},
		err:  os.ErrDeadlineExceeded,
	}
	chain := NewChainAlbumSource(partial, noneSource("b"))

	set, _ := chain.LocalAlbums(context.Background(), &Artist{Name: "Radiohead"})
	if set.Evidence != EvidenceFound {
		t.Errorf("Evidence = %v, want EvidenceFound: Evidence is the contract, not the error", set.Evidence)
	}
}

// TestChainAlbumSource_FillsMissingOrigin covers a source that neglects to set
// Origin: the chain names it, so a diagnostic never reads as coming from
// nowhere.
func TestChainAlbumSource_FillsMissingOrigin(t *testing.T) {
	anon := stubAlbumSource{name: "anon", set: AlbumSet{Evidence: EvidenceFound, Titles: []string{"Kid A"}}}
	set, _ := NewChainAlbumSource(anon).LocalAlbums(context.Background(), &Artist{Name: "Radiohead"})
	if set.Origin != "anon" {
		t.Errorf("Origin = %q, want %q", set.Origin, "anon")
	}
}

// TestCompareAlbumSet_DelegatesToCompareAlbums proves CompareAlbumSet carries no
// second copy of the matching logic: for a Found set its embedded comparison
// must be byte-identical to what CompareAlbums produces for the same inputs.
func TestCompareAlbumSet_DelegatesToCompareAlbums(t *testing.T) {
	local := []string{"OK Computer", "Kid A", "Amnesiac"}
	remote := []string{"OK Computer (Deluxe)", "Kid A", "Hail to the Thief"}

	want := CompareAlbums(local, remote)
	// Precondition: the fixture must actually exercise matching, or equality
	// below is satisfied by two empty structs.
	if want.MatchCount == 0 || want.MatchPercent == 0 || len(want.LocalOnly) == 0 || len(want.RemoteOnly) == 0 {
		t.Fatalf("precondition: fixture must produce matches, local-only AND remote-only rows; got %+v", want)
	}

	got := CompareAlbumSet(AlbumSet{Titles: local, Evidence: EvidenceFound, Origin: "filesystem"}, remote)

	if got.Evidence != EvidenceFound {
		t.Errorf("Evidence = %v, want EvidenceFound", got.Evidence)
	}
	if got.MatchCount != want.MatchCount || got.MatchPercent != want.MatchPercent ||
		got.LocalCount != want.LocalCount || got.RemoteCount != want.RemoteCount ||
		len(got.Matches) != len(want.Matches) || len(got.LocalOnly) != len(want.LocalOnly) ||
		len(got.RemoteOnly) != len(want.RemoteOnly) {
		t.Errorf("embedded comparison = %+v, want identical to CompareAlbums output %+v", got.AlbumComparison, want)
	}
}

// TestCompareAlbumSet_NonFoundContributesNoTitles pins the reason Evidence must
// be read instead of MatchPercent: an Unknown local side and a genuinely empty
// one produce the SAME arithmetic, and only Evidence tells them apart.
func TestCompareAlbumSet_NonFoundContributesNoTitles(t *testing.T) {
	remote := []string{"Kid A", "Amnesiac"}

	// Titles are deliberately populated on the Unknown set: a set that says
	// "I do not know" must not have its stale titles compared even if present.
	unknown := CompareAlbumSet(AlbumSet{Titles: []string{"Kid A"}, Evidence: EvidenceUnknown}, remote)
	none := CompareAlbumSet(AlbumSet{Evidence: EvidenceNone}, remote)

	if unknown.Evidence != EvidenceUnknown {
		t.Errorf("Evidence = %v, want EvidenceUnknown", unknown.Evidence)
	}
	if unknown.LocalCount != 0 || unknown.MatchCount != 0 || unknown.MatchPercent != 0 {
		t.Errorf("an Unknown local side must contribute no titles, got %+v", unknown.AlbumComparison)
	}
	if none.Evidence != EvidenceNone {
		t.Errorf("Evidence = %v, want EvidenceNone", none.Evidence)
	}

	// The whole point: the two are arithmetically indistinguishable.
	if unknown.MatchPercent != none.MatchPercent || unknown.LocalCount != none.LocalCount {
		t.Fatal("precondition failed: Unknown and None were expected to be arithmetically identical, which is why Evidence must be consulted")
	}
	if unknown.Evidence == none.Evidence {
		t.Error("Evidence must distinguish the two otherwise-identical comparisons")
	}
}

// TestListLocalAlbumsStillCollapsesErrors pins the existing behavior PR 2 will
// migrate away from. Six call sites depend on it, so it must not change under
// this refactor: an unreadable path and an empty one both return nil.
func TestListLocalAlbumsStillCollapsesErrors(t *testing.T) {
	empty := t.TempDir()
	if got := ListLocalAlbums(empty); got != nil {
		t.Errorf("ListLocalAlbums(empty dir) = %v, want nil", got)
	}
	if got := ListLocalAlbums(filepath.Join(empty, "does-not-exist")); got != nil {
		t.Errorf("ListLocalAlbums(missing dir) = %v, want nil", got)
	}
}

// TestEvidencedComparisonJSONShapeUnchanged pins the reason AlbumComparison is
// EMBEDDED rather than nested: the marshaled object must carry the existing keys
// at the top level and add nothing, so a handler can return an
// EvidencedComparison where it returned an AlbumComparison without breaking a
// client. Evidence is intentionally not on the wire in this PR.
func TestEvidencedComparisonJSONShapeUnchanged(t *testing.T) {
	local := []string{"Kid A", "Amnesiac"}
	remote := []string{"Kid A"}

	plain, err := json.Marshal(CompareAlbums(local, remote))
	if err != nil {
		t.Fatalf("marshal AlbumComparison: %v", err)
	}
	evidenced, err := json.Marshal(CompareAlbumSet(AlbumSet{Titles: local, Evidence: EvidenceFound}, remote))
	if err != nil {
		t.Fatalf("marshal EvidencedComparison: %v", err)
	}

	// Precondition: the fixture must produce a non-trivial object, or two
	// empty JSON documents would compare equal for the wrong reason.
	if !bytes.Contains(plain, []byte(`"match_percent":50`)) {
		t.Fatalf("precondition: fixture must produce a real comparison, got %s", plain)
	}

	if !bytes.Equal(plain, evidenced) {
		t.Errorf("JSON shape changed:\n AlbumComparison    = %s\n EvidencedComparison = %s", plain, evidenced)
	}
}
