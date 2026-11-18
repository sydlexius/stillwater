package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/platform"
)

// startListeners must launch the #2513 platform hash backfill, and that launch
// must be a goroutine (the pass sleeps 60s first). A behavioral test cannot boot
// the listener, so this is a source-level guard like the MBID sweep's.
//
// Mutation-proof: deleting a.startPlatformHashBackfill(ctx, db) from
// startListeners, or the `go ...StartPlatformHashBackfill` in
// startPlatformHashBackfill, makes this test FAIL.
func TestStartListenersLaunchesPlatformHashBackfill(t *testing.T) {
	file, _ := parseMainGo(t)
	listeners := findFunc(file, "startListeners")
	if listeners == nil {
		t.Fatal("could not find func startListeners in main.go -- has it been renamed?")
	}
	if !callsSelector(listeners.Body, "startPlatformHashBackfill") {
		t.Fatal("startListeners no longer calls startPlatformHashBackfill -- the #2513 pass " +
			"would be built but never launched")
	}
	starter := findFunc(file, "startPlatformHashBackfill")
	if starter == nil {
		t.Fatal("could not find func startPlatformHashBackfill in main.go -- has it been renamed?")
	}
	if !callsSelector(starter.Body, "StartPlatformHashBackfill") {
		t.Fatal("startPlatformHashBackfill no longer calls maintenance StartPlatformHashBackfill")
	}
}

// A nil active profile (GetActive's "none" answer) must be an error, never nil
// names: nil names fall back to the defaults and break the pass's fail-closed
// contract.
func TestProfileImageNames(t *testing.T) {
	ctx := context.Background()
	if _, err := profileImageNames(func(context.Context) (*platform.Profile, error) {
		return nil, nil
	})(ctx, "thumb"); err == nil {
		t.Error("nil profile returned no error; the row would be treated as having no local file")
	}
	boom := errors.New("database is locked")
	if _, err := profileImageNames(func(context.Context) (*platform.Profile, error) {
		return nil, boom
	})(ctx, "thumb"); !errors.Is(err, boom) {
		t.Errorf("GetActive error = %v, want it propagated", err)
	}
	names, err := profileImageNames(func(context.Context) (*platform.Profile, error) {
		return &platform.Profile{ImageNaming: platform.ImageNaming{Thumb: []string{"cover.jpg"}}}, nil
	})(ctx, "thumb")
	if err != nil || !slices.Equal(names, []string{"cover.jpg"}) {
		t.Errorf("names = %v, %v; want [cover.jpg]", names, err)
	}
}
