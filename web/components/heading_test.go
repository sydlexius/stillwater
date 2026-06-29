package components

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestHeadingLevelDefaults verifies the level helpers fall back to the stable
// defaults (card <h2>, sub-head <h3>) when no level is threaded through context,
// and clamp out-of-range values rather than emitting an invalid tag.
func TestHeadingLevelDefaults(t *testing.T) {
	cases := []struct {
		name        string
		ctx         context.Context
		wantCard    int
		wantSubHead int
	}{
		{"unset", context.Background(), 2, 3},
		{"next", WithHeadingLevel(context.Background(), 3), 3, 4},
		{"too-low-clamps", WithHeadingLevel(context.Background(), 1), 2, 3},
		{"too-high-clamps", WithHeadingLevel(context.Background(), 9), 2, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SettingCardHeadingLevel(c.ctx); got != c.wantCard {
				t.Errorf("SettingCardHeadingLevel = %d, want %d", got, c.wantCard)
			}
			if got := SettingSubHeadingLevel(c.ctx); got != c.wantSubHead {
				t.Errorf("SettingSubHeadingLevel = %d, want %d", got, c.wantSubHead)
			}
		})
	}
}

// TestHeadingEmitsLevelTag verifies the templ heading components emit the tag
// matching the context level, with the class passed through verbatim (the
// byte-stable contract the golden tests rely on).
func TestHeadingEmitsLevelTag(t *testing.T) {
	cases := []struct {
		name    string
		ctx     context.Context
		cardTag string
		subTag  string
	}{
		{"default", context.Background(), "h2", "h3"},
		{"next", WithHeadingLevel(context.Background(), 3), "h3", "h4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var card bytes.Buffer
			if err := SettingCardHeading("text-lg font-semibold", "Title").Render(c.ctx, &card); err != nil {
				t.Fatalf("card render: %v", err)
			}
			wantCard := "<" + c.cardTag + ` class="text-lg font-semibold">Title</` + c.cardTag + ">"
			if !strings.Contains(card.String(), wantCard) {
				t.Errorf("card heading = %q, want it to contain %q", card.String(), wantCard)
			}

			var sub bytes.Buffer
			if err := SettingSubHeading("text-sm", "Sub").Render(c.ctx, &sub); err != nil {
				t.Fatalf("sub render: %v", err)
			}
			wantSub := "<" + c.subTag + ` class="text-sm">Sub</` + c.subTag + ">"
			if !strings.Contains(sub.String(), wantSub) {
				t.Errorf("sub heading = %q, want it to contain %q", sub.String(), wantSub)
			}
		})
	}
}
