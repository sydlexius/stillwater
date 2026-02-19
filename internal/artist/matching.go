package artist

// MatchStrategy determines how artist matching prioritizes different signals.
type MatchStrategy string

// Match strategies.
const (
	MatchStrategyPreferID     MatchStrategy = "prefer_id"
	MatchStrategyPreferName   MatchStrategy = "prefer_name"
	MatchStrategyAlwaysPrompt MatchStrategy = "always_prompt"
)

// MatchType describes how the match was found.
type MatchType string

// Match types.
const (
	MatchTypeMBID     MatchType = "musicbrainz_id"
	MatchTypeAudioDB  MatchType = "audiodb_id"
	MatchTypeDiscogs  MatchType = "discogs_id"
	MatchTypeWikidata MatchType = "wikidata_id"
	MatchTypeName     MatchType = "name"
)

// MatchResult holds the outcome of an artist matching attempt.
type MatchResult struct {
	Artist     *Artist
	Confidence float64
	MatchType  MatchType
	Source     string
}

// MatchConfig holds configuration for the matching engine.
type MatchConfig struct {
	Strategy      MatchStrategy
	MinConfidence float64
}

// DefaultMatchConfig returns the default matching configuration.
func DefaultMatchConfig() MatchConfig {
	return MatchConfig{
		Strategy:      MatchStrategyPreferID,
		MinConfidence: 0.85,
	}
}
