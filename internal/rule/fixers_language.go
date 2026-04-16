package rule

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// NameLanguageFixer promotes a localized MusicBrainz alias into the artist's
// Name and SortName fields when the user's language preferences point at a
// higher-priority alias than the canonical name. Used by the
// name_language_pref rule.
//
// The fixer relies on the orchestrator (the same dependency used by
// MetadataFixer) to fetch artist metadata; the MusicBrainz adapter performs
// the actual alias scoring and promotion when the request context carries
// language preferences (provider.WithMetadataLanguages).
type NameLanguageFixer struct {
	orchestrator MetadataProvider
	logger       *slog.Logger
}

// NewNameLanguageFixer creates a NameLanguageFixer. The orchestrator is the
// same provider.Orchestrator passed to other metadata fixers.
func NewNameLanguageFixer(orchestrator MetadataProvider, logger *slog.Logger) *NameLanguageFixer {
	return &NameLanguageFixer{orchestrator: orchestrator, logger: logger}
}

// CanFix reports whether this fixer handles the given violation. Only the
// name_language_pref rule is supported.
func (f *NameLanguageFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNameLanguagePref
}

// Fix promotes the best-matching localized alias to a.Name and a.SortName.
// The orchestrator is invoked with the request context so the MusicBrainz
// adapter sees the user's language preferences and returns a localized name.
//
// Updates to Name and SortName are paired: when the alias has a non-empty
// SortName different from the canonical, both fields are updated together.
// When the alias has only a Name (no SortName), only Name is updated and
// SortName is preserved (the MB adapter applies the same rule).
func (f *NameLanguageFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if a.MusicBrainzID == "" {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: fmt.Sprintf("artist %s has no MusicBrainz ID", a.Name),
		}, nil
	}
	if a.Locked {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: fmt.Sprintf("artist %s is locked", a.Name),
		}, nil
	}
	if f.orchestrator == nil {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: "metadata orchestrator not configured",
		}, nil
	}
	if len(provider.MetadataLanguages(ctx)) == 0 {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: "no language preferences set; cannot promote localized name",
		}, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := f.orchestrator.FetchMetadata(fetchCtx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	if err != nil {
		return nil, fmt.Errorf("fetching metadata: %w", err)
	}
	if result == nil || result.Metadata == nil {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: fmt.Sprintf("no metadata returned for %s", a.Name),
		}, nil
	}

	bestName := strings.TrimSpace(result.Metadata.Name)
	bestSort := strings.TrimSpace(result.Metadata.SortName)

	// Treat the orchestrator's already-localized values as the source of
	// truth: they were chosen using the same MatchLanguagePreference scoring
	// as the checker, so trusting them keeps the two paths consistent.
	nameDiff := bestName != "" && bestName != a.Name
	sortDiff := bestSort != "" && bestSort != a.SortName

	if !nameDiff && !sortDiff {
		return &FixResult{
			RuleID:  RuleNameLanguagePref,
			Fixed:   false,
			Message: fmt.Sprintf("no localized alias differs from current name for %s", a.Name),
		}, nil
	}

	oldName := a.Name
	oldSort := a.SortName
	if nameDiff {
		a.Name = bestName
	}
	if sortDiff {
		a.SortName = bestSort
	}

	f.logger.Info("name_language_pref: promoted localized name",
		slog.String("artist_id", a.ID),
		slog.String("from_name", oldName),
		slog.String("to_name", a.Name),
		slog.String("from_sort", oldSort),
		slog.String("to_sort", a.SortName))

	return &FixResult{
		RuleID:  RuleNameLanguagePref,
		Fixed:   true,
		Message: fmt.Sprintf("promoted localized name '%s' (sort '%s') for %s", a.Name, a.SortName, oldName),
	}, nil
}
