package components

import "context"

// headingLevelKey carries the BASE heading level for settings section CARDS
// across a render. It lets the shared Section* / SettingSection content render
// the same card title as an <h2> on the stable Settings page (where each card
// sits directly under the page <h1>) but as an <h3> on the next/ Settings page
// (#1339 A2/A1), where an extra group-divider <h2> tier (Essentials / Data /
// Integrations / System) sits between the page <h1> and the cards. Threading it
// through context keeps a valid, non-skipping heading outline on BOTH pages
// without forking the shared content or changing any call-site signature.
type headingLevelKey struct{}

// WithHeadingLevel returns a context carrying base as the settings card-title
// heading level. The next/ Settings handler sets 3; the stable handler leaves it
// unset (default 2), so stable output is byte-identical to before this change.
func WithHeadingLevel(ctx context.Context, base int) context.Context {
	return context.WithValue(ctx, headingLevelKey{}, base)
}

// SettingCardHeadingLevel is the level for a section CARD title (h2 default / h3
// next). Clamped to [2,4] so a stray value can never emit an invalid tag.
func SettingCardHeadingLevel(ctx context.Context) int {
	if v, ok := ctx.Value(headingLevelKey{}).(int); ok && v >= 2 && v <= 4 {
		return v
	}
	return 2
}

// SettingSubHeadingLevel is the level for an in-card SUB-head (one below the
// card title): h3 on stable (base 2), h4 on next/ (base 3). This keeps the
// established stable output (sub-heads were hand-coded h3) byte-identical while
// nesting correctly one level under the card on next/.
func SettingSubHeadingLevel(ctx context.Context) int {
	return SettingCardHeadingLevel(ctx) + 1
}
