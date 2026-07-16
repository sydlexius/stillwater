package rule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// inScopeProviderIDs is the fixed set of non-MusicBrainz provider IDs the
// provider_id_missing rule can require. It is deliberately limited to the three
// providers whose IDs both (a) matter for artwork search and (b) are derivable
// from a MusicBrainz URL relation, so the fix has a source (issue #2457):
//
//   - AudioDB is excluded: its adapter accepts a bare MBID
//     (provider.ProviderAcceptsMBID), so a missing AudioDB ID never silently
//     skips it during image search.
//   - Wikidata is excluded: it is not an image provider and MusicBrainz exposes
//     no artwork relation to derive its ID from for this rule's purposes.
var inScopeProviderIDs = []provider.ProviderName{
	provider.NameDiscogs,
	provider.NameDeezer,
	provider.NameSpotify,
}

// InScopeProviderIDs returns a copy of the fixed set of non-MusicBrainz
// provider IDs the provider_id_missing rule can require (Discogs/Deezer/
// Spotify). It exists so the settings UI can render one config toggle per
// in-scope provider without duplicating the authoritative set; the copy keeps
// callers from mutating the package-level source of truth.
func InScopeProviderIDs() []provider.ProviderName {
	out := make([]provider.ProviderName, len(inScopeProviderIDs))
	copy(out, inScopeProviderIDs)
	return out
}

// providerIDForName returns the artist's stored ID for one of the in-scope
// providers. It returns "" for any provider outside the in-scope set, which is
// treated as "no ID" by the checker.
func providerIDForName(a *artist.Artist, name provider.ProviderName) string {
	switch name {
	case provider.NameDiscogs:
		return a.DiscogsID
	case provider.NameDeezer:
		return a.DeezerID
	case provider.NameSpotify:
		return a.SpotifyID
	default:
		return ""
	}
}

// parseProviderIDSet parses the comma-separated RequiredProviderIDs override
// into a set of recognized in-scope provider names. Unknown or out-of-scope
// tokens are ignored so a stray value cannot require a provider the rule cannot
// act on. Matching is case-insensitive and whitespace-trimmed.
func parseProviderIDSet(csv string) map[provider.ProviderName]bool {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	inScope := make(map[provider.ProviderName]bool, len(inScopeProviderIDs))
	for _, n := range inScopeProviderIDs {
		inScope[n] = true
	}
	out := make(map[provider.ProviderName]bool)
	for _, tok := range strings.Split(csv, ",") {
		name := provider.ProviderName(strings.ToLower(strings.TrimSpace(tok)))
		if name != "" && inScope[name] {
			out[name] = true
		}
	}
	return out
}

// expectedProviderIDs resolves the set of provider IDs an artist is required to
// carry, implementing the dynamic-default design (DC1, issue #2457):
//
//	expected = (configured providers) ∩ {Discogs, Deezer, Spotify}
//
// narrowed further to the operator's RequiredProviderIDs override when one is
// set. The result is sorted so the resulting violation message is deterministic.
//
// The provider-availability dependency is threaded a context and its error is
// handled by the caller: an unwired dependency yields an empty set (the rule
// no-ops), and a lookup error is surfaced to the caller so the checker can
// degrade to a no-op rather than panic or emit a misleading violation.
func (e *Engine) expectedProviderIDs(ctx context.Context, cfg RuleConfig) ([]provider.ProviderName, error) {
	if e.providerAvailability == nil {
		return nil, nil
	}
	available, err := e.providerAvailability.AvailableProviderNames(ctx)
	if err != nil {
		return nil, err
	}

	override := parseProviderIDSet(cfg.RequiredProviderIDs)

	expected := make([]provider.ProviderName, 0, len(inScopeProviderIDs))
	for _, name := range inScopeProviderIDs {
		if !available[name] {
			continue
		}
		if len(override) > 0 && !override[name] {
			continue
		}
		expected = append(expected, name)
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i] < expected[j] })
	return expected, nil
}

// makeProviderIDMissingChecker returns a Checker that flags an artist missing
// one or more of the required non-MusicBrainz provider IDs. The required set is
// the configured providers among Discogs/Deezer/Spotify, optionally narrowed by
// the rule's RequiredProviderIDs override.
//
// The rule is detection-only here; ProviderIDBackfillFixer performs the
// MusicBrainz-derived backfill. It is not filesystem-dependent: provider IDs
// matter for artwork search whether or not the artist has a local path.
func (e *Engine) makeProviderIDMissingChecker() Checker {
	return func(ctx context.Context, a *artist.Artist, cfg RuleConfig) *Violation {
		expected, err := e.expectedProviderIDs(ctx, cfg)
		if err != nil {
			// Degrade to a no-op: without a reliable availability set the rule
			// cannot decide what to require, and flagging (or not) on a guess
			// would be misleading. The failure is logged so it is recoverable.
			e.logger.Warn("provider_id_missing: cannot resolve available providers; skipping",
				slog.String("artist", a.Name),
				slog.String("error", err.Error()))
			return nil
		}
		if len(expected) == 0 {
			return nil
		}

		// expected is sorted, so missing inherits that order and the message is
		// deterministic across runs.
		var missing []string
		for _, name := range expected {
			if strings.TrimSpace(providerIDForName(a, name)) == "" {
				missing = append(missing, string(name))
			}
		}
		if len(missing) == 0 {
			return nil
		}

		return &Violation{
			RuleID:   RuleProviderIDMissing,
			RuleName: "Provider IDs present",
			Category: string(RuleCategoryMetadata),
			Severity: effectiveSeverity(cfg),
			Message: fmt.Sprintf(
				"artist %s is missing provider IDs for image search: %s",
				a.Name, strings.Join(missing, ", ")),
			Fixable: true,
		}
	}
}
