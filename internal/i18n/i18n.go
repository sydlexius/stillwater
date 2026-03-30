// Package i18n provides internationalization support for user-facing strings.
// Phase 1 ships English only; the translation layer is in place for community
// translations in future phases.
package i18n

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// contextKey is an unexported type for context keys in this package.
type contextKey string

const translatorKey contextKey = "i18n.translator"

// Translator holds loaded translation strings for a single locale.
type Translator struct {
	locale  string
	strings map[string]string
}

// NewTranslator creates a Translator with the given locale and a defensive copy
// of the string map. This is useful for testing or programmatic construction.
// If locale is empty, it defaults to "en".
func NewTranslator(locale string, src map[string]string) *Translator {
	if locale == "" {
		locale = "en"
	}
	m := make(map[string]string, len(src))
	for k, v := range src {
		m[k] = v
	}
	return &Translator{locale: locale, strings: m}
}

// Locale returns the locale code for this translator (e.g. "en").
func (t *Translator) Locale() string {
	return t.locale
}

// T returns the translated string for the given key. If the key is not found,
// the key itself is returned as a fallback so missing translations are visible
// in the UI rather than silently blank.
func (t *Translator) T(key string) string {
	if v, ok := t.strings[key]; ok {
		return v
	}
	return key
}

// Tn returns a pluralized translation. It looks up "key.one" when count is 1
// and "key.other" for all other counts. The count is substituted for the
// placeholder {count} in the translated string.
func (t *Translator) Tn(key string, count int) string {
	suffix := "other"
	if count == 1 {
		suffix = "one"
	}
	fullKey := key + "." + suffix
	template := t.T(fullKey)
	return strings.ReplaceAll(template, "{count}", strconv.Itoa(count))
}

// Bundle holds translators for all loaded locales and provides locale
// negotiation.
type Bundle struct {
	translators map[string]*Translator
	fallback    string
}

// Load reads all JSON locale files from localesDir and returns a Bundle.
// Each file must be named "<locale>.json" (e.g. "en.json") and contain a flat
// JSON object mapping string keys to translated values.
//
// Nested keys use dot notation in the JSON keys themselves:
//
//	{"nav.dashboard": "Dashboard", "actions.count.one": "{count} action"}
//
// At least one locale file must be present. The fallback locale is "en"; if
// "en" is not among the loaded files, the first locale alphabetically is used.
func Load(localesDir string) (*Bundle, error) {
	entries, err := os.ReadDir(localesDir)
	if err != nil {
		return nil, fmt.Errorf("i18n: reading locales directory: %w", err)
	}

	b := &Bundle{
		translators: make(map[string]*Translator),
		fallback:    "en",
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		locale := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(localesDir, entry.Name())

		data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from os.ReadDir entries within the application-provided locales directory, not user input
		if err != nil {
			return nil, fmt.Errorf("i18n: reading %s: %w", path, err)
		}

		var translations map[string]string
		if err := json.Unmarshal(data, &translations); err != nil {
			return nil, fmt.Errorf("i18n: parsing %s: %w", path, err)
		}

		b.translators[locale] = &Translator{locale: locale, strings: translations}
		slog.Info("loaded locale", "locale", locale, "keys", len(translations))
	}

	if len(b.translators) == 0 {
		return nil, fmt.Errorf("i18n: no locale files found in %s", localesDir)
	}

	// If the default fallback is not available, pick the first alphabetically.
	if _, ok := b.translators[b.fallback]; !ok {
		locales := make([]string, 0, len(b.translators))
		for l := range b.translators {
			locales = append(locales, l)
		}
		sort.Strings(locales)
		b.fallback = locales[0]
	}

	return b, nil
}

// Translator returns the Translator for the given locale. If the locale is not
// loaded, the fallback translator is returned.
func (b *Bundle) Translator(locale string) *Translator {
	if t, ok := b.translators[locale]; ok {
		return t
	}
	return b.translators[b.fallback]
}

// Fallback returns the fallback locale code.
func (b *Bundle) Fallback() string {
	return b.fallback
}

// Locales returns all loaded locale codes sorted alphabetically.
func (b *Bundle) Locales() []string {
	locales := make([]string, 0, len(b.translators))
	for l := range b.translators {
		locales = append(locales, l)
	}
	sort.Strings(locales)
	return locales
}

// ParseAcceptLanguage parses an Accept-Language header value and returns the
// best matching locale from the bundle. If no match is found, the fallback
// locale is returned.
//
// The parser handles weighted entries (e.g. "de;q=0.9, en;q=0.8") and returns
// the highest-weighted locale that exists in the bundle.
func (b *Bundle) ParseAcceptLanguage(header string) string {
	if header == "" {
		return b.fallback
	}

	type entry struct {
		locale string
		weight float64
	}

	var entries []entry
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		locale := part
		weight := 1.0

		if idx := strings.Index(part, ";"); idx >= 0 {
			locale = strings.TrimSpace(part[:idx])
			qPart := strings.TrimSpace(part[idx+1:])
			if strings.HasPrefix(qPart, "q=") {
				if w, err := strconv.ParseFloat(qPart[2:], 64); err == nil {
					// Clamp to [0, 1] per RFC 9110.
					if w < 0 {
						w = 0
					} else if w > 1 {
						w = 1
					}
					weight = w
				}
			}
		}

		// RFC 9110: q=0 means "not acceptable" -- skip this locale entirely.
		if weight == 0 {
			continue
		}

		// Normalize: take only the primary language subtag (e.g. "en-US" -> "en").
		if idx := strings.Index(locale, "-"); idx >= 0 {
			locale = locale[:idx]
		}
		locale = strings.ToLower(locale)

		entries = append(entries, entry{locale: locale, weight: weight})
	}

	// Sort by weight descending (stable to preserve header order for equal weights).
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].weight > entries[j].weight
	})

	for _, e := range entries {
		if _, ok := b.translators[e.locale]; ok {
			return e.locale
		}
	}

	return b.fallback
}

// WithTranslator stores a Translator in the context.
func WithTranslator(ctx context.Context, t *Translator) context.Context {
	return context.WithValue(ctx, translatorKey, t)
}

// TFromCtx retrieves the Translator from the context. If no translator is
// present (e.g. middleware not configured), it logs a warning and returns a
// default English translator with an empty string map so that T() calls fall
// back to returning the key itself.
func TFromCtx(ctx context.Context) *Translator {
	if t, ok := ctx.Value(translatorKey).(*Translator); ok && t != nil {
		return t
	}
	slog.Warn("i18n: no translator in context, returning empty fallback")
	return &Translator{locale: "en", strings: make(map[string]string)}
}
