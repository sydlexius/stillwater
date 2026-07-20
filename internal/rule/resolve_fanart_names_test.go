package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/platform"
)

// TestResolveFanartNames pins the convention-agnostic enumeration name list
// (#2635). resolveFanartNames must return EVERY name a library could have used
// for fanart -- the active profile's names UNIONED with the built-in defaults,
// profile first, deduplicated case-insensitively -- so a walk built from it can
// never come up empty against a directory that holds artwork under a different
// convention than the one the profile writes under.
//
// Every case asserts the OUTCOME: the exact ordered name slice, not that a line
// ran.
func TestResolveFanartNames(t *testing.T) {
	ctx := context.Background()

	// The built-in defaults, in order. resolveFanartNames appends these after
	// the profile's names.
	defaults := []string{"fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"}

	t.Run("nil platform service yields the built-in defaults", func(t *testing.T) {
		got, err := resolveFanartNames(ctx, nil)
		if err != nil {
			t.Fatalf("resolveFanartNames: %v", err)
		}
		assertNames(t, got, defaults)
	})

	t.Run("profile primary that is also a default is deduped to the front", func(t *testing.T) {
		// "backdrop.jpg" is both the profile's primary AND a default. The union
		// must list it once, in the profile's leading position, not twice.
		svc := activeProfileWithFanart(t, "backdrop.jpg")
		got, err := resolveFanartNames(ctx, svc)
		if err != nil {
			t.Fatalf("resolveFanartNames: %v", err)
		}
		assertNames(t, got, []string{"backdrop.jpg", "fanart.jpg", "fanart.png", "backdrop.png"})
	})

	t.Run("profile primary absent from the defaults leads the union", func(t *testing.T) {
		// A convention the defaults do not know about must still be enumerated,
		// ahead of the defaults, or artwork written under it walks invisible.
		svc := activeProfileWithFanart(t, "art.jpg")
		got, err := resolveFanartNames(ctx, svc)
		if err != nil {
			t.Fatalf("resolveFanartNames: %v", err)
		}
		assertNames(t, got, []string{"art.jpg", "fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"})
	})
}

// activeProfileWithFanart returns a platform service whose active profile names
// fanart with exactly the given filename.
func activeProfileWithFanart(t *testing.T, fanart string) *platform.Service {
	t.Helper()
	ctx := context.Background()
	svc := platform.NewService(setupTestDB(t))
	p := &platform.Profile{
		Name:      "test-fanart-naming",
		NFOFormat: "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: []string{fanart},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
		IsActive: true,
	}
	if err := svc.Create(ctx, p); err != nil {
		t.Fatalf("creating profile: %v", err)
	}
	if err := svc.SetActive(ctx, p.ID); err != nil {
		t.Fatalf("activating profile: %v", err)
	}
	return svc
}

func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
