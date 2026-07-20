package artist

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// This file is the regression suite for issue #2635, in which a single
// shared bulk-write primitive silently destroyed artist_images state: it
// deleted every stored slot absent from the incoming slice, and that slice was
// derived purely from an Artist struct's flat image fields. Any caller that
// reached Service.Update holding an Artist whose image fields were not
// populated therefore wiped rows for artwork that was still on disk.
//
// Every test here asserts DATABASE state read back through GetImagesForArtist
// rather than a return value or a counter, because the failure mode being
// guarded is precisely a call that reports success while destroying data.
// Each test also asserts its seed state before acting, so that a fixture which
// silently stopped seeding rows cannot let the test pass vacuously.

// seedGuardArtist creates an artist plus an explicit set of image rows and
// verifies the seed landed. It returns the service and the created artist.
func seedGuardArtist(t *testing.T, name string, images []ArtistImage) (*Service, *Artist, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist(name, "/music/"+name)
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := range images {
		img := images[i]
		img.ArtistID = a.ID
		if err := svc.UpsertImage(ctx, &img); err != nil {
			t.Fatalf("seeding image %s/%d: %v", img.ImageType, img.SlotIndex, err)
		}
	}

	got, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("seed verify GetImagesForArtist: %v", err)
	}
	if len(got) != len(images) {
		t.Fatalf("seed precondition: wanted %d image rows, got %d (fixture is not seeding; "+
			"every assertion below would pass vacuously)", len(images), len(got))
	}
	return svc, a, db
}

// slotSet reduces the artist's stored rows to a "type/slot" -> exists map so
// assertions can name a slot directly instead of indexing a sorted slice.
func slotSet(t *testing.T, svc *Service, artistID string) map[string]bool {
	t.Helper()
	imgs, err := svc.GetImagesForArtist(context.Background(), artistID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	out := make(map[string]bool, len(imgs))
	for _, im := range imgs {
		out[im.ImageType+"/"+strconv.Itoa(im.SlotIndex)] = im.Exists
	}
	return out
}

func requireSlots(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("slot %s was destroyed; stored slots are %v", k, keysOf(got))
		}
	}
	if len(got) != len(want) {
		t.Errorf("stored slot count = %d, want %d (stored: %v, wanted: %v)",
			len(got), len(want), keysOf(got), want)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestUpdate_UnderPopulatedArtistPreservesImages is the images-side mirror of
// TestRenameDirectory_PreservesProviderIDs, which has guarded the provider
// half of persistNormalized since May. The image half never had one, and #2635
// is the incident that gap allowed.
//
// An Artist whose image fields are all zero says NOTHING about what is on
// disk. Flowing it through Update must therefore leave every stored row alone.
// Before the fix this call deleted all four rows.
func TestUpdate_UnderPopulatedArtistPreservesImages(t *testing.T) {
	t.Parallel()
	svc, a, _ := seedGuardArtist(t, "UnderPopulated", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
		{ImageType: "logo", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()

	// A bare Artist carrying only identity and a changed name: exactly the
	// shape produced by a non-hydrating load or a field-level edit. Every
	// image field is left at its zero value.
	bare := &Artist{
		ID:       a.ID,
		Name:     "Under Populated Renamed",
		SortName: "Under Populated Renamed",
		Path:     a.Path,
	}
	if err := svc.Update(ctx, bare); err != nil {
		t.Fatalf("Update: %v", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "fanart/0", "fanart/1", "logo/0")
}

// TestUpdate_StaleFanartExistsFalseKeepsTail covers the amplification path.
// applyImageMetadata derives FanartExists from slot 0 alone, and
// extractImageMetadata gates slots 1..N behind that single flag, so ONE stale
// or cleared slot-0 flag used to delete the entire fanart tail. Slots 1 and 2
// describe different files that the caller said nothing about; they must
// survive regardless of what slot 0 claims.
func TestUpdate_StaleFanartExistsFalseKeepsTail(t *testing.T) {
	t.Parallel()
	svc, a, _ := seedGuardArtist(t, "FanartTail", []ArtistImage{
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
		{ImageType: "fanart", SlotIndex: 2, Exists: true},
	})
	ctx := context.Background()

	stale := &Artist{
		ID:           a.ID,
		Name:         a.Name,
		SortName:     a.SortName,
		Path:         a.Path,
		FanartExists: false, // the stale slot-0 flag that used to truncate
		FanartCount:  0,
	}
	if err := svc.Update(ctx, stale); err != nil {
		t.Fatalf("Update: %v", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "fanart/0", "fanart/1", "fanart/2")
}

// canonicalEnumeration builds an enumeration over every canonical image type,
// defaulting any type not named in `found` to zero files found. Test-only: it
// models a caller that walked the whole artist directory, which is the
// scanner's position and nobody else's.
func canonicalEnumeration(found map[string]int) []ImageEnumeration {
	out := make([]ImageEnumeration, 0, len(CanonicalImageTypes))
	for _, t := range CanonicalImageTypes {
		out = append(out, ImageEnumeration{ImageType: t, FoundSlots: found[t]})
	}
	return out
}

// The next two tests are a PAIR, and the pairing is the point. A low
// FanartCount has two completely different meanings depending on who is
// carrying it, and the fix is only correct if the two are told apart:
//
//   - a declarative caller (Update) with a low count has not looked at the
//     filesystem, so its count is silence and must delete nothing;
//   - a caller that genuinely walked the directory and counted two files has
//     positively verified that ordinals 2 and 3 do not exist, and MUST
//     converge, or deleted artwork is rendered forever.
//
// An earlier version of this suite asserted only the first and framed a low
// count as categorically "a caller that has not looked". That is false for the
// enumerating callers -- their count is low precisely BECAUSE files were
// deleted -- so the assertion pinned the strand-forever bug as intended
// behavior and would have blocked the fix for it.

// TestUpdate_StaleLowFanartCountKeepsHigherSlots is the declarative half. An
// Artist carrying FanartExists=true but a stale, low FanartCount emits slots
// 0..Count-1 only; the slots above used to be read as absent and deleted.
// Update acts on no absence, so they must survive.
func TestUpdate_StaleLowFanartCountKeepsHigherSlots(t *testing.T) {
	t.Parallel()
	svc, a, _ := seedGuardArtist(t, "FanartCount", []ArtistImage{
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
		{ImageType: "fanart", SlotIndex: 2, Exists: true},
		{ImageType: "fanart", SlotIndex: 3, Exists: true},
	})
	ctx := context.Background()

	stale := &Artist{
		ID:           a.ID,
		Name:         a.Name,
		SortName:     a.SortName,
		Path:         a.Path,
		FanartExists: true,
		FanartCount:  2, // stale: this caller never looked; disk still has 4
	}
	if err := svc.Update(ctx, stale); err != nil {
		t.Fatalf("Update: %v", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "fanart/0", "fanart/1", "fanart/2", "fanart/3")
}

// TestReconcileImages_EnumeratedLowFanartCountConverges is the enumerating
// half, and the positive control for every caller migrated in this change
// (updateArtistFanartCount, the phash back-out, the fanart-duplicate
// remediation). The artist carries the SAME low count as the test above; the
// only difference is that this caller walked the directory and counted, and
// that difference must decide the outcome.
//
// Without this, "never delete" passes every guard test in this file while
// leaving rows for deleted artwork on screen permanently.
func TestReconcileImages_EnumeratedLowFanartCountConverges(t *testing.T) {
	t.Parallel()
	svc, a, _ := seedGuardArtist(t, "FanartCountEnumerated", []ArtistImage{
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
		{ImageType: "fanart", SlotIndex: 2, Exists: true},
		{ImageType: "fanart", SlotIndex: 3, Exists: true},
	})
	ctx := context.Background()

	enumerated := &Artist{
		ID:           a.ID,
		Name:         a.Name,
		SortName:     a.SortName,
		Path:         a.Path,
		FanartExists: true,
		FanartCount:  2, // measured: the operator deleted two files
	}
	repaired, err := svc.ReconcileImages(ctx, enumerated,
		[]ImageEnumeration{{ImageType: "fanart", FoundSlots: 2}})
	if err != nil {
		t.Fatalf("ReconcileImages: %v", err)
	}
	if !repaired {
		t.Error("repaired=false, but two rows had to be deleted; a false here also " +
			"suppresses the caller's ArtistUpdated fanout")
	}

	requireSlots(t, slotSet(t, svc, a.ID), "fanart/0", "fanart/1")
}

// TestReconcileAll_EmptyEnumerationRefusesAndDestroysNothing pins the refusal.
// A caller asking to converge while declaring no enumerated types has looked
// at nothing, so its empty slot set is evidence of nothing. That request is
// the exact shape of the #2635 incident and must fail loudly rather than wipe
// the artist or silently no-op.
func TestReconcileAll_EmptyEnumerationRefusesAndDestroysNothing(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "EmptyEnum", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	err := repo.ReconcileAll(ctx, a.ID, nil, nil)
	if err == nil {
		t.Fatal("ReconcileAll with no enumerated types returned nil; an unproven " +
			"whole-registry wipe must be refused, and a silent no-op would hide the caller bug")
	}
	if !errors.Is(err, ErrNoImageEnumeration) {
		t.Errorf("error = %v, want one wrapping ErrNoImageEnumeration so callers can "+
			"distinguish a caller-scope bug from a real DB failure", err)
	}

	// The refusal must have destroyed nothing. Asserting the error alone would
	// pass even if the rows had been deleted before the check.
	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "fanart/0")
}

// TestReconcileAll_EmptyEnumerationRefusalNamesTheArtist pins the attribution
// on the empty-enumeration refusal.
//
// The sibling test above proves the refusal fires and destroys nothing. It does
// not prove the refusal is actionable. A bare sentinel tells an operator that
// some reconcile somewhere passed no enumeration, which is exactly the report
// you cannot act on: this rejection is the one most likely to fire in
// production, and every instance of it reads identically in the log. The artist
// ID is the only handle on WHICH call went wrong.
//
// Unlike the three rejections below it in validateEnumeration, this one cannot
// name the offending entry, because an empty slice has no entry to name. The
// attribution has to be threaded in from the caller.
func TestReconcileAll_EmptyEnumerationRefusalNamesTheArtist(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "AttributedRefusal", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	err := repo.ReconcileAll(ctx, a.ID, nil, nil)
	if err == nil {
		t.Fatal("ReconcileAll with no enumeration returned nil; the refusal must fire")
	}

	// Identity first. Attribution added with a bare fmt.Errorf instead of %w
	// would read correctly in a log while silently breaking every caller that
	// branches on errors.Is -- a regression this assertion is here to catch.
	if !errors.Is(err, ErrNoImageEnumeration) {
		t.Errorf("error = %v, want one still wrapping ErrNoImageEnumeration; "+
			"adding attribution must not cost callers the ability to classify it", err)
	}

	// The attribution itself. Without the artist ID an operator holding this
	// log line cannot tell which reconcile misbehaved.
	if !strings.Contains(err.Error(), a.ID) {
		t.Errorf("error = %q, want it to name artist %q; an unattributed refusal "+
			"gives an operator nothing to act on, and this rejection is the one "+
			"most likely to fire in production", err.Error(), a.ID)
	}

	// Attribution is worthless if the refusal leaked. Assert the rows, never
	// just the error: the bug class here is code that reports a failure while
	// having already destroyed data.
	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "fanart/0")
}

// TestReconcileAll_EnumeratedCallerStillDeletes proves the guard did not
// simply freeze all deletion. A caller that genuinely enumerated the
// filesystem and found fanart slot 1 gone must still have that row removed --
// convergence to filesystem truth is the whole reason ReconcileAll exists.
func TestReconcileAll_EnumeratedCallerStillDeletes(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "RealReconcile", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// The caller probed all canonical types and found thumb/0 and fanart/0.
	if err := repo.ReconcileAll(ctx, a.ID, []ArtistImage{
		{ArtistID: a.ID, ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ArtistID: a.ID, ImageType: "fanart", SlotIndex: 0, Exists: true},
	}, canonicalEnumeration(map[string]int{"thumb": 1, "fanart": 1})); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "fanart/0")
}

// TestReconcileAll_EnumeratedEmptySetConverges is the operator who deleted all
// of an artist's artwork. The caller probed every canonical type and found
// nothing, which is a positive verified finding, so the registry must follow.
// This is the case that distinguishes the enumeration-scope design from a
// blanket "never accept an empty set" rule, which would strand these rows
// permanently and keep rendering artwork that no longer exists.
func TestReconcileAll_EnumeratedEmptySetConverges(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "AllArtworkGone", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	if err := repo.ReconcileAll(ctx, a.ID, nil, canonicalEnumeration(nil)); err != nil {
		t.Fatalf("ReconcileAll with a verified-empty enumeration: %v", err)
	}

	if got := slotSet(t, svc, a.ID); len(got) != 0 {
		t.Errorf("stored slots = %v, want none: the caller verified every canonical "+
			"type was absent, so the registry must converge", keysOf(got))
	}
}

// TestReconcileAll_ScopeBoundsDeletionToEnumeratedTypes proves the scope is a
// real bound and not a token that merely unlocks deletion. A caller that
// probed only fanart must not be able to take out rows of a type it never
// looked for, however absent those rows are from its slice.
func TestReconcileAll_ScopeBoundsDeletionToEnumeratedTypes(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "ScopeBound", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
		{ImageType: "poster", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Probed fanart only, and found just slot 0. fanart/1 is in scope and
	// absent, so it goes. thumb/0 and poster/0 were never examined and stay.
	if err := repo.ReconcileAll(ctx, a.ID, []ArtistImage{
		{ArtistID: a.ID, ImageType: "fanart", SlotIndex: 0, Exists: true},
	}, []ImageEnumeration{{ImageType: "fanart", FoundSlots: 1}}); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "fanart/0", "poster/0")
}

// TestReconcileAll_UnderReportedSliceCannotWipeVerifiedSlots is the incident
// mechanism itself, reproduced INSIDE the destructive path that was supposed
// to have contained it.
//
// Bounding deletion by image TYPE alone is not the property #2635 asks for.
// Within an enumerated type the incoming slice is re-derived from an Artist's
// flat fields by extractImageMetadata, which gates the entire fanart tail
// behind slot 0. So an artist holding fanart1.jpg with NO primary yields an
// incoming slice with ZERO fanart rows -- and a type-bounded reconcile reads
// that as "every fanart row is stale" and deletes them all, including the row
// for the file sitting on disk. Same amplification, new path.
//
// The enumeration's COUNT is what closes it: the caller walked the directory
// and found one file, which positively verifies ordinal 0 exists and ordinals
// 1+ do not, whatever the lossy slice says.
//
// This state is not exotic. A slot delete that fails partway skips
// renumbering and leaves exactly this shape (#2644).
func TestReconcileAll_UnderReportedSliceCannotWipeVerifiedSlots(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "Amplification", []ArtistImage{
		{ImageType: "fanart", SlotIndex: 0, Exists: true},
		{ImageType: "fanart", SlotIndex: 1, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Precondition: the incoming slice really is empty for fanart. Building it
	// the way production does proves the under-reporting is real and not a
	// hand-written stub, so this test cannot pass because the fixture was
	// generous.
	orphan := &Artist{
		ID:           a.ID,
		Name:         a.Name,
		Path:         a.Path,
		FanartExists: false, // no PRIMARY on disk...
		FanartCount:  1,     // ...but the walk counted one file: fanart1.jpg
	}
	desired := extractImageMetadata(orphan)
	for _, im := range desired {
		if im.ImageType == "fanart" {
			t.Fatalf("precondition failed: extractImageMetadata emitted a fanart row (%+v); "+
				"the slot-0 gating this test exists to survive is gone, so the test "+
				"would pass without proving anything", im)
		}
	}

	// The caller enumerated fanart and positively found ONE file.
	if err := repo.ReconcileAll(ctx, a.ID, desired,
		[]ImageEnumeration{{ImageType: "fanart", FoundSlots: 1}}); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	// Ordinal 0 is backed by a file the caller counted and must survive.
	// Ordinal 1 is past the count and is legitimately retired, so this is not
	// a "freeze everything" assertion.
	requireSlots(t, slotSet(t, svc, a.ID), "fanart/0")
}

// TestReconcileAll_EmptyEnumerationRefusesOnACleanRegistry closes the gap that
// let the refusal be data-dependent.
//
// The refusal used to fire only when the artist happened to be holding rows
// that would have been deleted. A caller that forgot its enumeration therefore
// got a silent nil against a clean registry and never learned it was broken --
// it found out later, in production, against an unlucky artist. The contract
// has to be a property of the CALL, so this artist deliberately holds rows
// that NOTHING would delete.
func TestReconcileAll_EmptyEnumerationRefusesOnACleanRegistry(t *testing.T) {
	t.Parallel()
	svc, a, db := seedGuardArtist(t, "CleanRegistryRefusal", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Every stored row is named in `images`, so the old data-dependent check
	// found zero delete candidates and returned nil.
	err := repo.ReconcileAll(ctx, a.ID, []ArtistImage{
		{ArtistID: a.ID, ImageType: "thumb", SlotIndex: 0, Exists: true},
	}, nil)
	if err == nil {
		t.Fatal("ReconcileAll with no enumeration returned nil because the registry " +
			"happened to be clean; the contract must not depend on the data it meets")
	}
	if !errors.Is(err, ErrNoImageEnumeration) {
		t.Errorf("error = %v, want one wrapping ErrNoImageEnumeration", err)
	}

	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0")
}

// TestReconcileAll_MalformedEnumerationRefuses covers the entries that cannot
// bound a delete: an unnamed type, a negative count, and two conflicting
// counts for one type. Each is a caller that did not really enumerate, and
// each must be refused rather than silently resolved to some default -- a
// default here would be a deletion decided by a coin flip.
func TestReconcileAll_MalformedEnumerationRefuses(t *testing.T) {
	t.Parallel()
	cases := map[string][]ImageEnumeration{
		"empty image type": {{ImageType: "", FoundSlots: 1}},
		"negative count":   {{ImageType: "fanart", FoundSlots: -1}},
		"conflicting counts for one type": {
			{ImageType: "fanart", FoundSlots: 1},
			{ImageType: "fanart", FoundSlots: 3},
		},
	}
	for name, enumerated := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, a, db := seedGuardArtist(t, "Malformed"+name, []ArtistImage{
				{ImageType: "fanart", SlotIndex: 0, Exists: true},
				{ImageType: "fanart", SlotIndex: 5, Exists: true},
			})
			repo := newSQLiteImageRepo(db)

			err := repo.ReconcileAll(context.Background(), a.ID, nil, enumerated)
			if !errors.Is(err, ErrNoImageEnumeration) {
				t.Errorf("error = %v, want one wrapping ErrNoImageEnumeration", err)
			}
			// fanart/5 is exactly the row a mishandled enumeration would take.
			requireSlots(t, slotSet(t, svc, a.ID), "fanart/0", "fanart/5")
		})
	}
}

// TestReconcileImages_IdempotentWithOutOfScopeRow pins the docstring's
// "replaying with the same Artist is a no-op" against the row that used to
// break it.
//
// The drift check compared ALL stored rows against a canonical-only desired
// set, so an artist holding a "poster" row reported drift forever: ReconcileAll
// correctly refuses to delete it, the next call sees the same difference, and
// around it goes. The cost is not cosmetic. The scanner publishes an
// ArtistUpdated event plus a full write transaction on every `repaired=true`,
// so a quiet rescan of such an artist flooded subscribers -- precisely what the
// `repaired` return exists to prevent.
func TestReconcileImages_IdempotentWithOutOfScopeRow(t *testing.T) {
	t.Parallel()
	svc, a, _ := seedGuardArtist(t, "PosterDrift", []ArtistImage{
		{ImageType: "thumb", SlotIndex: 0, Exists: true},
		{ImageType: "poster", SlotIndex: 0, Exists: true},
	})
	ctx := context.Background()

	model := &Artist{
		ID:          a.ID,
		Name:        a.Name,
		Path:        a.Path,
		ThumbExists: true,
	}
	enumerated := canonicalEnumeration(map[string]int{"thumb": 1})

	// First call may legitimately repair. Every call after it must not: the
	// state it would converge to is the state it is already in.
	if _, err := svc.ReconcileImages(ctx, model, enumerated); err != nil {
		t.Fatalf("ReconcileImages (first): %v", err)
	}
	for i := 2; i <= 4; i++ {
		repaired, err := svc.ReconcileImages(ctx, model, enumerated)
		if err != nil {
			t.Fatalf("ReconcileImages (call %d): %v", i, err)
		}
		if repaired {
			t.Fatalf("call %d reported repaired=true; ReconcileImages is documented "+
				"idempotent, and every true here is an ArtistUpdated event plus a "+
				"write transaction on an artist nothing changed about", i)
		}
	}

	// The poster row is out of the enumeration's reach and must be untouched
	// throughout -- idempotence achieved by deleting it would be a worse bug.
	requireSlots(t, slotSet(t, svc, a.ID), "thumb/0", "poster/0")
}
