package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/platform"
)

// discardResolverLogger returns a resolver logger plus the buffer it writes to,
// so a test can assert the warn branch actually reported rather than dropping
// the failure on the floor.
func resolverLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestFanartPrimaryResolver_ProfileReadFailureReturnsEmpty pins the branch that
// makes this whole task fail LOUDLY instead of silently.
//
// A GetActive error must yield "" -- never the default naming. The regression
// this catches is not a crash: it is the resolver answering "fanart.jpg" for a
// library whose fanart files are all backdrop.jpg. DiscoverFanart keys off that
// one name, so every starved slot is skipped, and the pass logs a clean run with
// filled=0 and no error while healing nothing. BackfillFanartHashes rejects an
// empty primary and retries next tick, which is the correct outcome for a
// transient read failure.
func TestFanartPrimaryResolver_ProfileReadFailureReturnsEmpty(t *testing.T) {
	logger, buf := resolverLogger()
	getActive := func(context.Context) (*platform.Profile, error) {
		return nil, errors.New("database is locked")
	}

	got := fanartPrimaryResolver(getActive, logger)(context.Background())

	if got != "" {
		t.Errorf("resolver returned %q on a GetActive error, want %q -- falling back to the "+
			"default naming makes an Emby library (backdrop.jpg) discover zero files, so the "+
			"pass skips every starved slot and reports a clean, error-free no-op", got, "")
	}
	if !strings.Contains(buf.String(), "reading active platform profile") {
		t.Errorf("no warning logged for a failed profile read; log = %q", buf.String())
	}
}

// TestFanartPrimaryResolver_NilProfileReturnsEmpty pins the same contract for a
// nil profile returned WITHOUT an error, which is a distinct branch and just as
// silent: there is no active profile, so no naming can be known, and guessing
// one is exactly the substitution the FanartPrimaryFn contract forbids.
func TestFanartPrimaryResolver_NilProfileReturnsEmpty(t *testing.T) {
	logger, buf := resolverLogger()
	getActive := func(context.Context) (*platform.Profile, error) {
		return nil, nil
	}

	got := fanartPrimaryResolver(getActive, logger)(context.Background())

	if got != "" {
		t.Errorf("resolver returned %q for a nil active profile, want %q", got, "")
	}
	if !strings.Contains(buf.String(), "no active platform profile") {
		t.Errorf("no warning logged for a nil active profile; log = %q", buf.String())
	}
}

// TestFanartPrimaryResolver_ActiveProfileNameWins asserts the happy path really
// reads the profile rather than the default, using Emby's backdrop.jpg -- the
// exact value the two tests above exist to stop being replaced by "fanart.jpg".
func TestFanartPrimaryResolver_ActiveProfileNameWins(t *testing.T) {
	logger, _ := resolverLogger()
	getActive := func(context.Context) (*platform.Profile, error) {
		return &platform.Profile{
			ImageNaming: platform.ImageNaming{Fanart: []string{"backdrop.jpg", "backdrop.png"}},
		}, nil
	}

	if got := fanartPrimaryResolver(getActive, logger)(context.Background()); got != "backdrop.jpg" {
		t.Errorf("resolver returned %q for an Emby profile, want %q", got, "backdrop.jpg")
	}
}

// TestFanartPrimaryResolver_ReadableProfileWithoutNameKeepsDefault guards the
// OTHER side of the contract, so the fix above cannot be over-applied.
//
// A profile that reads back fine but configures no fanart name is NOT an unknown
// profile -- it is an ordinary Kodi-shaped profile whose primary name genuinely
// is the default "fanart.jpg". Returning "" here would stall the backfill
// forever on a perfectly healthy configuration.
func TestFanartPrimaryResolver_ReadableProfileWithoutNameKeepsDefault(t *testing.T) {
	logger, _ := resolverLogger()
	getActive := func(context.Context) (*platform.Profile, error) {
		return &platform.Profile{ImageNaming: platform.ImageNaming{}}, nil
	}

	if got := fanartPrimaryResolver(getActive, logger)(context.Background()); got != "fanart.jpg" {
		t.Errorf("resolver returned %q for a readable profile with no custom fanart name, "+
			"want the default %q", got, "fanart.jpg")
	}
}
