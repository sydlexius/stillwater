package templates

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// allKeysConfigured marks every given provider as configured so the key
// availability filter never hides one; only the capability check can.
func allKeysConfigured(names ...provider.ProviderName) []provider.ProviderKeyStatus {
	keys := make([]provider.ProviderKeyStatus, 0, len(names))
	for _, n := range names {
		keys = append(keys, provider.ProviderKeyStatus{Name: n, Status: "ok"})
	}
	return keys
}

// TestAvailableProviders_CapabilityFilter pins issue #3190: the priority editor
// must not offer a provider whose declared capabilities cannot supply the field.
func TestAvailableProviders_CapabilityFilter(t *testing.T) {
	keys := allKeysConfigured(provider.NameAudioDB, provider.NameLastFM, provider.NameDiscogs,
		provider.NameFanartTV, provider.NameDeezer, provider.NameWikidata)

	tests := []struct {
		name      string
		field     string
		providers []provider.ProviderName
		want      []provider.ProviderName
	}{
		{"metadata field drops incapable provider", "moods",
			[]provider.ProviderName{provider.NameAudioDB, provider.NameLastFM, provider.NameDiscogs},
			[]provider.ProviderName{provider.NameAudioDB, provider.NameLastFM}},
		{"image field drops provider without that image type", "fanart",
			[]provider.ProviderName{provider.NameFanartTV, provider.NameDiscogs, provider.NameDeezer},
			[]provider.ProviderName{provider.NameFanartTV}},
		{"image field keeps capable providers", "thumb",
			[]provider.ProviderName{provider.NameFanartTV, provider.NameDiscogs, provider.NameLastFM},
			[]provider.ProviderName{provider.NameFanartTV, provider.NameDiscogs}},
		{"web search provider bypasses the check", "fanart",
			[]provider.ProviderName{provider.NameFanartTV, provider.NameDuckDuckGo},
			[]provider.ProviderName{provider.NameFanartTV, provider.NameDuckDuckGo}},
		{"years_active keeps AudioDB via synthesis", "years_active",
			[]provider.ProviderName{provider.NameAudioDB, provider.NameDiscogs},
			[]provider.ProviderName{provider.NameAudioDB}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := availableProviders(provider.FieldPriority{Field: tc.field, Providers: tc.providers}, keys)
			if !slices.Equal(got, tc.want) {
				t.Errorf("availableProviders(%s) = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}

// TestPriorityChipRow_OmitsIncapableProvider renders the real chip row and
// asserts a provider saved in the priority list but unable to supply the field
// is not offered. This is the rendered-HTML stand-in for browser evidence.
func TestPriorityChipRow_OmitsIncapableProvider(t *testing.T) {
	ctx := testCtx(t)
	pri := provider.FieldPriority{
		Field:     "moods",
		Providers: []provider.ProviderName{provider.NameAudioDB, provider.NameLastFM, provider.NameDiscogs},
	}
	keys := allKeysConfigured(provider.NameAudioDB, provider.NameLastFM, provider.NameDiscogs)
	var buf bytes.Buffer
	if err := PriorityChipRow(pri, keys).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if strings.Contains(html, `data-provider="discogs"`) {
		t.Errorf("moods row offers Discogs, which cannot supply moods; rendered=%s", html)
	}
	for _, want := range []string{`data-provider="audiodb"`, `data-provider="lastfm"`} {
		if !strings.Contains(html, want) {
			t.Errorf("moods row missing %s; rendered=%s", want, html)
		}
	}
}

// TestPriorityChipRow_KeepsHiddenMarkerForIncapableProvider pins F1 of the #3190
// review: a stored provider rendered as no chip must leave a hidden marker, outside
// the sortable container, so sortable-init.js can keep it in the saved order.
func TestPriorityChipRow_KeepsHiddenMarkerForIncapableProvider(t *testing.T) {
	ctx := testCtx(t)
	pri := provider.FieldPriority{
		Field:     "moods",
		Providers: []provider.ProviderName{provider.NameAudioDB, provider.NameDiscogs, provider.NameLastFM},
	}
	// LastFM has no key row, so it is hidden for the no-key reason.
	keys := allKeysConfigured(provider.NameAudioDB, provider.NameDiscogs)
	var buf bytes.Buffer
	if err := PriorityChipRow(pri, keys).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	container := html[strings.Index(html, "data-sortable-field"):]
	container = container[:strings.Index(container, "</div>")]
	for _, name := range []string{"discogs", "lastfm"} {
		marker := `data-hidden-provider="` + name + `"`
		if !strings.Contains(html, marker) {
			t.Errorf("missing hidden marker %s; rendered=%s", marker, html)
		}
		if strings.Contains(container, marker) {
			t.Errorf("hidden marker %s is inside the sortable container", marker)
		}
	}
	if strings.Contains(html, `data-hidden-provider="audiodb"`) {
		t.Errorf("rendered chip audiodb must not get a hidden marker")
	}
}

// TestAvailableProviders_EveryDefaultRowKeepsEveryProvider runs every default
// priority row through the filter with all keys configured and asserts nothing is
// dropped: a deleted image-mapping entry would hide a whole logo/banner row.
func TestAvailableProviders_EveryDefaultRowKeepsEveryProvider(t *testing.T) {
	rows := provider.DefaultPriorities()
	if len(rows) == 0 {
		t.Fatal("DefaultPriorities() is empty; the sweep would pass vacuously")
	}
	var names []provider.ProviderName
	for provName := range provider.ProviderCapabilities() {
		names = append(names, provName)
	}
	keys := allKeysConfigured(names...)
	for _, row := range rows {
		got := availableProviders(row, keys)
		if !slices.Equal(got, row.Providers) {
			t.Errorf("default row %q: availableProviders = %v, want %v", row.Field, got, row.Providers)
		}
	}
}

func TestAvailableProviders_UnknownProviderHidden(t *testing.T) {
	keys := allKeysConfigured("allmusic", provider.NameAudioDB)
	got := availableProviders(provider.FieldPriority{Field: "genres", Providers: []provider.ProviderName{"allmusic", provider.NameAudioDB}}, keys)
	if !slices.Equal(got, []provider.ProviderName{provider.NameAudioDB}) {
		t.Errorf("availableProviders = %v, want [audiodb]", got)
	}
}
