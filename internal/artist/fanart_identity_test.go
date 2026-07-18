package artist

// fanart_identity_test.go covers BuildFanartIdentityIndex, the registry
// loader half of #2540's shared phash-identity foundation. See
// internal/image/identity_test.go for the comparison-primitive half.

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/image"
)

func TestBuildFanartIdentityIndex(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	a := testArtist("Fanart Usable", "/music/Fanart Usable")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b := testArtist("Fanart NonFanart", "/music/Fanart NonFanart")
	if err := svc.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := testArtist("Fanart NotExists", "/music/Fanart NotExists")
	if err := svc.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	d := testArtist("Fanart Unusable", "/music/Fanart Unusable")
	if err := svc.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// a: a usable fanart row -- must be included.
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: a.ID, ImageType: "fanart", SlotIndex: 0, Exists: true,
		PHash: "0f0f0f0f0f0f0f0f",
	}); err != nil {
		t.Fatalf("Upsert a fanart: %v", err)
	}

	// b: a usable THUMB row, no fanart -- must be excluded (fanart-only
	// registry; see BuildFanartIdentityIndex doc for why thumb is not
	// pulled in here the way the #2564 detector's report registry does).
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: b.ID, ImageType: "thumb", SlotIndex: 0, Exists: true,
		PHash: "1111111111111111",
	}); err != nil {
		t.Fatalf("Upsert b thumb: %v", err)
	}

	// c: a fanart row that does NOT exist on disk (exists_flag=0) -- must
	// be excluded even though it carries a hash.
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: c.ID, ImageType: "fanart", SlotIndex: 0, Exists: false,
		PHash: "2222222222222222",
	}); err != nil {
		t.Fatalf("Upsert c fanart: %v", err)
	}

	// d: three fanart rows exercising every unusable-hash case: empty
	// (never hashed), unparsable, and the zero sentinel. None must appear
	// in the index.
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: d.ID, ImageType: "fanart", SlotIndex: 0, Exists: true,
		PHash: "",
	}); err != nil {
		t.Fatalf("Upsert d empty: %v", err)
	}
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: d.ID, ImageType: "fanart", SlotIndex: 1, Exists: true,
		PHash: "not-hex",
	}); err != nil {
		t.Fatalf("Upsert d unparsable: %v", err)
	}
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: d.ID, ImageType: "fanart", SlotIndex: 2, Exists: true,
		PHash: "0000000000000000",
	}); err != nil {
		t.Fatalf("Upsert d zero: %v", err)
	}
	// d also gets one usable fanart row, to prove the exclusions above are
	// per-row, not "drop the whole artist because one row was bad".
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: d.ID, ImageType: "fanart", SlotIndex: 3, Exists: true,
		PHash: "3333333333333333",
	}); err != nil {
		t.Fatalf("Upsert d usable: %v", err)
	}

	got, err := svc.BuildFanartIdentityIndex(ctx)
	if err != nil {
		t.Fatalf("BuildFanartIdentityIndex: %v", err)
	}

	want := map[string]uint64{
		a.ID: 0x0f0f0f0f0f0f0f0f,
		d.ID: 0x3333333333333333,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	seen := map[string]uint64{}
	for _, e := range got {
		if _, dup := seen[e.ArtistID]; dup {
			t.Fatalf("artist %s appeared more than once in the index: %+v", e.ArtistID, got)
		}
		seen[e.ArtistID] = e.PHash
	}
	for artistID, wantHash := range want {
		gotHash, ok := seen[artistID]
		if !ok {
			t.Errorf("artist %s missing from index; got %+v", artistID, got)
			continue
		}
		if gotHash != wantHash {
			t.Errorf("artist %s PHash = %x, want %x", artistID, gotHash, wantHash)
		}
	}
	if _, present := seen[b.ID]; present {
		t.Errorf("thumb-only artist %s leaked into the fanart-only index", b.ID)
	}
	if _, present := seen[c.ID]; present {
		t.Errorf("exists_flag=0 artist %s leaked into the index", c.ID)
	}
}

func TestBuildFanartIdentityIndex_EmptyLibrary(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	got, err := svc.BuildFanartIdentityIndex(ctx)
	if err != nil {
		t.Fatalf("BuildFanartIdentityIndex: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries from an empty library, want 0: %+v", len(got), got)
	}
}

// TestBuildFanartIdentityIndex_FeedsCompareIdentity is a thin integration
// check that the loader's output shape is actually consumable by
// image.CompareIdentity without adaptation -- the whole point of sharing the
// FanartIdentityEntry type between the two packages.
func TestBuildFanartIdentityIndex_FeedsCompareIdentity(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	owner := testArtist("Owner", "/music/Owner")
	if err := svc.Create(ctx, owner); err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: owner.ID, ImageType: "fanart", SlotIndex: 0, Exists: true,
		PHash: "aaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("Upsert owner fanart: %v", err)
	}

	index, err := svc.BuildFanartIdentityIndex(ctx)
	if err != nil {
		t.Fatalf("BuildFanartIdentityIndex: %v", err)
	}

	candidate, err := image.ParseHashHex("aaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseHashHex: %v", err)
	}
	result := image.CompareIdentity(candidate, "some-other-artist", index, 0.90)
	if result.Verdict != image.IdentityMismatch {
		t.Fatalf("Verdict = %v, want IdentityMismatch (identical hash to a different artist's fanart)", result.Verdict)
	}
	if result.CollidingArtistID != owner.ID {
		t.Errorf("CollidingArtistID = %q, want %q", result.CollidingArtistID, owner.ID)
	}
}
