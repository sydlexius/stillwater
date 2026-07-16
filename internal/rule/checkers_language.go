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

// makeNameLanguagePrefChecker returns a Checker that flags artists whose
// stored Name or SortName is in a script that does not match any of the
// user's preferred metadata languages. Both fields are validated
// independently so a mismatch on either raises a violation.
//
// Detection uses Unicode script analysis (v1): non-Latin vs Latin mismatches
// are caught reliably. Latin-vs-Latin (e.g. German vs English) is out of
// scope -- the rule cannot distinguish those without a language detector.
//
// When a MusicBrainz alias in a preferred language is available, the violation
// is marked fixable. When no alias exists (or no MBID, or MB lookup fails),
// the violation is still raised but marked unfixable so the user can edit
// manually or dismiss.
func (e *Engine) makeNameLanguagePrefChecker() Checker {
	return func(ctx context.Context, a *artist.Artist, cfg RuleConfig) *Violation {
		if a.Locked || strings.TrimSpace(a.Name) == "" {
			return nil
		}

		langPrefs := provider.MetadataLanguages(ctx)
		if len(langPrefs) == 0 {
			e.logger.Debug("name_language_pref: no language preferences in context, skipping",
				slog.String("artist", a.Name))
			return nil
		}

		nameScript := provider.DominantScript(a.Name)
		nameOK := nameScript == provider.ScriptUnknown || provider.ScriptMatchesAnyLocale(nameScript, langPrefs)

		sortScript := provider.DominantScript(a.SortName)
		sortOK := sortScript == provider.ScriptUnknown || provider.ScriptMatchesAnyLocale(sortScript, langPrefs)

		if nameOK && sortOK {
			return nil
		}

		// Use the mismatched script for the message; prefer Name's script
		// since it is the primary display field.
		script := nameScript
		if nameOK {
			script = sortScript
		}

		prefList := strings.Join(langPrefs, ", ")

		aliasName, aliasSort := e.lookupPreferredAlias(ctx, a)

		// The violation is fixable only when the alias can repair every
		// out-of-policy field. If Name is in the wrong script, the alias
		// must supply a different Name; same for SortName. Otherwise the
		// Fix button would leave part of the mismatch behind, causing the
		// violation to reopen on the next evaluation.
		nameFixable := nameOK || (aliasName != "" && aliasName != a.Name)
		sortFixable := sortOK || (aliasSort != "" && aliasSort != a.SortName)
		fixable := nameFixable && sortFixable

		var msg string
		if fixable {
			switch {
			case !nameOK && !sortOK && strings.TrimSpace(a.SortName) != "":
				msg = fmt.Sprintf("artist name '%s' (sort '%s') does not match preferred languages [%s]; localized alias '%s' (sort '%s') available",
					a.Name, a.SortName, prefList, aliasName, aliasSort)
			case !nameOK:
				msg = fmt.Sprintf("artist name '%s' does not match preferred languages [%s]; localized alias '%s' available",
					a.Name, prefList, aliasName)
			default:
				msg = fmt.Sprintf("artist sort name '%s' does not match preferred languages [%s]; localized sort '%s' available",
					a.SortName, prefList, aliasSort)
			}
		} else {
			switch {
			case !nameOK && !sortOK && strings.TrimSpace(a.SortName) != "":
				msg = fmt.Sprintf("artist name '%s' (sort '%s') does not match preferred languages [%s]; no localized alias available -- edit manually or dismiss",
					a.Name, a.SortName, prefList)
			case !nameOK:
				msg = fmt.Sprintf("artist name '%s' is in %s script but preferred languages are [%s]; no localized alias available -- edit manually or dismiss",
					a.Name, script, prefList)
			default:
				msg = fmt.Sprintf("artist sort name '%s' is in %s script but preferred languages are [%s]; no localized alias available -- edit manually or dismiss",
					a.SortName, script, prefList)
			}
		}

		return &Violation{
			RuleID:   RuleNameLanguagePref,
			RuleName: "Artist name matches preferred language",
			Category: "metadata",
			Severity: effectiveSeverity(cfg),
			Message:  msg,
			Fixable:  fixable,
		}
	}
}

// lookupPreferredAlias returns the localized Name and SortName (if any)
// from MusicBrainz that match the user's language preferences. The caller
// decides whether each field is fixable by comparing against the stored
// values. Returns empty strings when the provider is unwired, the MBID is
// missing, or the lookup fails or times out.
func (e *Engine) lookupPreferredAlias(ctx context.Context, a *artist.Artist) (aliasName, aliasSort string) {
	prov := e.metadataProviderFor(RuleNameLanguagePref)
	if a.MusicBrainzID == "" || prov == nil {
		return "", ""
	}

	// The orchestrator may query multiple providers (MB, Wikipedia, etc.),
	// which is far heavier than the alias check we need here. Cap the lookup
	// so evaluation does not block the request for 30+ seconds; degrade to
	// empty result on timeout rather than hanging.
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	fr, err := e.fetchMetadataCoalesced(fetchCtx, prov, a)
	if err != nil {
		e.logger.Warn("name_language_pref: metadata fetch failed",
			slog.String("artist", a.Name),
			slog.String("mbid", a.MusicBrainzID),
			slog.String("error", err.Error()))
		return "", ""
	}
	if fr == nil || fr.Metadata == nil {
		return "", ""
	}

	return strings.TrimSpace(fr.Metadata.Name), strings.TrimSpace(fr.Metadata.SortName)
}

// fetchMetadataCoalesced routes the alias metadata fetch through the per-artist
// EvaluationContext coalescer when one is attached to ctx (the canonical
// pipeline path), so the pre-fix and post-fix name_language_pref checker passes
// of a single scoped run collapse to one provider call (#2476). When no
// EvaluationContext is present it falls back to the bare provider, preserving
// the legacy behavior. In production the EvaluationContext's orchestrator and
// the metadata provider are the same *provider.Orchestrator (both wired from
// a.orchestrator in main.go), so routing through the coalescer hits the
// identical upstream. prov is the capability-gated handle the caller already
// obtained via metadataProviderFor; it is passed in rather than read off the
// Engine so the capability gate stays the single access point.
func (e *Engine) fetchMetadataCoalesced(ctx context.Context, prov MetadataProvider, a *artist.Artist) (*provider.FetchResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	}
	return prov.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
}
