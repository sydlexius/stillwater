package templates

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

var hxValsAttr = regexp.MustCompile(`hx-vals="([^"]*)"`)

// TestFieldProviderModalContent_ProducerClaims pins which provider-modal
// buttons claim a producer (#3078). "Use this" sends exactly the provider's
// value, so it claims provider:<name>. "Merge" sends the artist's existing
// values combined with the provider's, a mixed-provenance list, so it must
// claim nothing and record unrecorded rather than credit one provider.
func TestFieldProviderModalContent_ProducerClaims(t *testing.T) {
	a := &artist.Artist{ID: "a-3078", Name: "Test Band", Genres: []string{"Pop"}}
	results := []provider.FieldProviderResult{
		{Provider: provider.NameLastFM, HasData: true, Values: []string{"Rock", "Jazz"}},
	}

	var buf bytes.Buffer
	if err := FieldProviderModalContent(a, "genres", results, "Pop").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	var withProducer, withoutProducer []string
	for _, m := range hxValsAttr.FindAllStringSubmatch(buf.String(), -1) {
		vals := html.UnescapeString(m[1])
		if strings.Contains(vals, `"producer"`) {
			withProducer = append(withProducer, vals)
		} else {
			withoutProducer = append(withoutProducer, vals)
		}
	}
	if len(withProducer) != 1 || !strings.Contains(withProducer[0], `"producer":"provider:lastfm"`) ||
		strings.Contains(withProducer[0], "Pop") {
		t.Errorf("want exactly one button claiming provider:lastfm with the provider-only value; got %v", withProducer)
	}
	if len(withoutProducer) != 1 || !strings.Contains(withoutProducer[0], "Pop") {
		t.Errorf("want exactly one Merge button (existing + provider values) with no producer claim; got %v", withoutProducer)
	}
}
