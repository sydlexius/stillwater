package rule

import "context"

// fakeHashRecorder is an imageHashRecorder that records what it was asked to do
// without touching a database. Fixers require a recorder (renumbering without
// one is refused, loudly, by image.RenumberFanart), so tests that exercise a
// fixer's file handling but not its persistence supply this instead of nil.
//
// It counts invalidations so a test can assert that a renumber actually dropped
// the artist's stale hashes rather than merely returning a nil error.
type fakeHashRecorder struct {
	updates       int
	invalidated   []string // "artistID/imageType" per InvalidateImageHashes call
	invalidateErr error
}

func (f *fakeHashRecorder) UpdateImageHashes(_ context.Context, _, _ string, _ int, _, _ string) error {
	f.updates++
	return nil
}

func (f *fakeHashRecorder) InvalidateImageHashes(_ context.Context, artistID, imageType string) error {
	f.invalidated = append(f.invalidated, artistID+"/"+imageType)
	return f.invalidateErr
}
