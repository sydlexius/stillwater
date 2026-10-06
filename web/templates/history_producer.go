package templates

import (
	"context"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// history_producer.go -- display rules for the recorded value producer on a
// history row (issue #3078, slice 4a). The vocabulary lives in
// internal/artist/history_producer.go; this file only turns it into words.
//
// A history row has two independent facts. The source badge says what STARTED
// the write; the producer says what SUPPLIED THE VALUE. The label here must
// never mislead, so:
//
//   - An empty producer is NEVER hidden. "" means the writer did not record it
//     (every row from before tracking, plus deliberate unknowns), and absence
//     must not read as clean. It is also never rendered as "set by a user".
//   - The label is hidden ONLY when it would repeat the badge (producer equals
//     source, or an Undo whose value is a restore), and on the synthetic
//     rule_fix pseudo-field, which is an audit message and not a value.
//   - provider:identify_* are match tiers, not providers: never "from <x>".
//   - A token this build does not know is shown verbatim, never mapped to a
//     guessed label and never rendered as an empty "Value: from ".
//   - Meaning is carried by the text, not by color.

// historyProducerLabel returns the chip text for a recorded producer.
func historyProducerLabel(ctx context.Context, producer string) string {
	switch producer {
	case artist.ProducerUnrecorded:
		return t(ctx, "history.producer.unrecorded")
	case artist.ProducerOperator:
		return t(ctx, "history.producer.operator")
	case artist.ProducerRestore:
		return t(ctx, "history.producer.restore")
	case "nfo":
		return t(ctx, "history.producer.nfo")
	case "filesystem":
		return t(ctx, "history.producer.filesystem")
	case "provider:identify_connection":
		return t(ctx, "history.producer.identify_connection")
	case "provider:identify_album":
		return t(ctx, "history.producer.identify_album")
	case "provider:identify_name":
		return t(ctx, "history.producer.identify_name")
	case "provider:":
		return t(ctx, "history.producer.provider_unnamed")
	}
	if name, ok := strings.CutPrefix(producer, "provider:"); ok {
		return tf(ctx, "history.producer.from", provider.ProviderName(name).DisplayName())
	}
	if name, ok := strings.CutPrefix(producer, "platform:"); ok && name != "" {
		return tf(ctx, "history.producer.from", displayNameOrRaw(sourceDisplayName(name), name))
	}
	if id, ok := strings.CutPrefix(producer, "rule:"); ok && id != "" {
		return tf(ctx, "history.producer.rule_named", id)
	}
	return producer
}

// displayNameOrRaw falls back to the raw token when no display name exists.
func displayNameOrRaw(display, raw string) string {
	if display == "" {
		return raw
	}
	return display
}

// historyProducerShown reports whether the producer chip renders for c.
func historyProducerShown(c artist.MetadataChange) bool {
	if c.Field == "rule_fix" {
		return false
	}
	if c.Producer != "" && c.Producer == c.Source {
		return false
	}
	return c.Source != "revert" || c.Producer != artist.ProducerRestore
}

// historyProducerChipClass reuses the neutral source-badge classes (already
// contrast-measured in both themes); the not-recorded state is italic.
func historyProducerChipClass(producer string) string {
	cls := "inline-block px-2 py-0.5 rounded-full text-xs " + historySourceBadgeClass("")
	if producer == artist.ProducerUnrecorded {
		cls += " italic"
	}
	return cls
}
