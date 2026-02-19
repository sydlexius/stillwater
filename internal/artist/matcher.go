package artist

import "context"

// Matcher performs artist matching using configured strategies.
type Matcher struct {
	service *Service
	config  MatchConfig
}

// NewMatcher creates a matcher with the given service and configuration.
func NewMatcher(service *Service, config MatchConfig) *Matcher {
	return &Matcher{
		service: service,
		config:  config,
	}
}

// MatchByMBID attempts to find an artist by MusicBrainz ID.
// Returns nil result if no match found.
func (m *Matcher) MatchByMBID(ctx context.Context, mbid string) (*MatchResult, error) {
	if mbid == "" {
		return nil, nil
	}

	a, err := m.service.GetByMBID(ctx, mbid)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil
	}

	return &MatchResult{
		Artist:     a,
		Confidence: 1.0,
		MatchType:  MatchTypeMBID,
		Source:     "local_db",
	}, nil
}

// MatchByID attempts to find an artist by any provider ID.
// It tries each provider in priority order and returns the first match.
func (m *Matcher) MatchByID(ctx context.Context, ids map[string]string) (*MatchResult, error) {
	// Try providers in priority order
	providers := []struct {
		key       string
		matchType MatchType
	}{
		{"musicbrainz", MatchTypeMBID},
		{"audiodb", MatchTypeAudioDB},
		{"discogs", MatchTypeDiscogs},
		{"wikidata", MatchTypeWikidata},
	}

	for _, p := range providers {
		id, ok := ids[p.key]
		if !ok || id == "" {
			continue
		}

		a, err := m.service.GetByProviderID(ctx, p.key, id)
		if err != nil {
			return nil, err
		}
		if a == nil {
			continue
		}

		confidence := 1.0
		if p.matchType != MatchTypeMBID {
			confidence = 0.95
		}

		return &MatchResult{
			Artist:     a,
			Confidence: confidence,
			MatchType:  p.matchType,
			Source:     "local_db",
		}, nil
	}

	return nil, nil
}

// Match attempts to find an artist using the configured strategy.
// In M2, only ID-based matching is implemented.
// Name-based matching will be added in M3.
func (m *Matcher) Match(ctx context.Context, ids map[string]string, _ string) (*MatchResult, error) {
	switch m.config.Strategy {
	case MatchStrategyPreferID, MatchStrategyPreferName:
		// Both strategies try ID first in M2
		result, err := m.MatchByID(ctx, ids)
		if err != nil {
			return nil, err
		}
		if result != nil && result.Confidence >= m.config.MinConfidence {
			return result, nil
		}
		return nil, nil

	case MatchStrategyAlwaysPrompt:
		// Still perform the lookup, but return it for review
		result, err := m.MatchByID(ctx, ids)
		if err != nil {
			return nil, err
		}
		return result, nil

	default:
		return m.MatchByID(ctx, ids)
	}
}
