package templates

// PreferencesData holds the current user preference values shown in the
// Appearance settings tab. All fields are pre-populated from the user_preferences
// table (or the compiled defaults) by the settings page handler.
type PreferencesData struct {
	Theme             string // "dark" | "light" | "system"
	ThumbnailSize     string // "small" | "medium" | "large"
	SidebarState      string // "full" | "icon-only" | "hidden"
	ContentWidth      string // "narrow" | "wide"
	ReducedMotion     string // "system" | "on" | "off"
	Language          string // "en" (Phase 1 only)
	FontFamily        string // "system" | "inter" | "atkinson"
	LetterSpacing     string // "normal" | "wide" | "extra-wide"
	FontSize          string // "small" | "medium" | "large"
	LiteMode          string // "off" | "on" | "auto"
	PageSize          int    // Validated page size in the range 10-500; defaults to 50.
	AutoFetchImages   string // "true" | "false" -- auto-search providers on image search.
	BackgroundOpacity string // "20"-"100" -- background glass opacity percentage.

	// M55 #1774: preferences flyout drawer keys.
	Density             string // "compact" | "comfortable" | "spacious"
	MonoFont            string // "system" | "jetbrains" | "cascadia"
	KbdHints            string // "show" | "hide"
	NotificationEnabled string // "true" | "false"
	// M55 #2377: touch floor. "on" lifts every icon-only control to the 44px
	// touch target. Off by default: coarse-pointer devices already get it from
	// the (pointer: coarse) media query. This is for the touchscreen LAPTOP,
	// which reports pointer: fine and therefore cannot be detected.
	TouchFriendly string // "off" | "on"
	// M55 #2060: per-user debug tab toggle (migrated from global app setting).
	ShowPlatformDebug string // "true" | "false"
	// Artist detail layout (written via PATCH; nil = server default).
	ArtistDetailSectionOrder      []string
	ArtistDetailHiddenSections    []string
	ArtistDetailCollapsedSections []string
}
