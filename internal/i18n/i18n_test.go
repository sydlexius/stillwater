package i18n

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidLocaleFile(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"greeting": "Hello", "farewell": "Goodbye"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	tr := bundle.Translator("en")
	if got := tr.T("greeting"); got != "Hello" {
		t.Errorf("T(greeting) = %q, want %q", got, "Hello")
	}
	if got := tr.T("farewell"); got != "Goodbye" {
		t.Errorf("T(farewell) = %q, want %q", got, "Goodbye")
	}
}

func TestLoad_MultipleLocales(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"greeting": "Hello"}`)
	writeLocaleFile(t, dir, "de.json", `{"greeting": "Hallo"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	locales := bundle.Locales()
	if len(locales) != 2 {
		t.Fatalf("Locales() returned %d, want 2", len(locales))
	}

	if got := bundle.Translator("de").T("greeting"); got != "Hallo" {
		t.Errorf("de T(greeting) = %q, want %q", got, "Hallo")
	}
}

func TestLoad_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load should return error for empty directory")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"broken":}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load should return error for invalid JSON")
	}
}

func TestT_MissingKeyReturnsKey(t *testing.T) {
	tr := NewTranslator("en", map[string]string{"exists": "value"})

	if got := tr.T("missing.key"); got != "missing.key" {
		t.Errorf("T(missing.key) = %q, want %q", got, "missing.key")
	}
}

func TestTn_Pluralization(t *testing.T) {
	tr := NewTranslator("en", map[string]string{
		"items.one":   "{count} item",
		"items.other": "{count} items",
	})

	tests := []struct {
		count int
		want  string
	}{
		{1, "1 item"},
		{0, "0 items"},
		{5, "5 items"},
		{100, "100 items"},
	}

	for _, tt := range tests {
		got := tr.Tn("items", tt.count)
		if got != tt.want {
			t.Errorf("Tn(items, %d) = %q, want %q", tt.count, got, tt.want)
		}
	}
}

func TestTn_MissingPluralKeyFallsBackToKey(t *testing.T) {
	tr := NewTranslator("en", map[string]string{})

	// When plural keys are missing, the full key (e.g. "things.other") is returned.
	got := tr.Tn("things", 5)
	if got != "things.other" {
		t.Errorf("Tn(things, 5) = %q, want %q", got, "things.other")
	}
}

func TestParseAcceptLanguage(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"k": "v"}`)
	writeLocaleFile(t, dir, "de.json", `{"k": "w"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"empty header", "", "en"},
		{"exact match", "de", "de"},
		{"with region", "de-DE", "de"},
		{"weighted prefer de", "en;q=0.8, de;q=0.9", "de"},
		{"weighted prefer en", "en;q=0.9, de;q=0.8", "en"},
		{"unknown locale falls back", "fr", "en"},
		{"wildcard ignored", "*", "en"},
		{"mixed known and unknown", "fr, de;q=0.5", "de"},
		{"case insensitive", "EN-US", "en"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bundle.ParseAcceptLanguage(tt.header)
			if got != tt.want {
				t.Errorf("ParseAcceptLanguage(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestTFromCtx_WithTranslator(t *testing.T) {
	tr := NewTranslator("en", map[string]string{"key": "value"})
	ctx := WithTranslator(context.Background(), tr)

	got := TFromCtx(ctx)
	if got != tr {
		t.Error("TFromCtx did not return the stored translator")
	}
	if got.T("key") != "value" {
		t.Errorf("TFromCtx translator T(key) = %q, want %q", got.T("key"), "value")
	}
}

func TestTFromCtx_EmptyContext(t *testing.T) {
	got := TFromCtx(context.Background())
	if got == nil {
		t.Fatal("TFromCtx returned nil for empty context")
	}
	if got.Locale() != "en" {
		t.Errorf("default translator locale = %q, want %q", got.Locale(), "en")
	}
	// Missing keys should return the key itself.
	if got.T("anything") != "anything" {
		t.Errorf("default translator T(anything) = %q, want %q", got.T("anything"), "anything")
	}
}

func TestBundleTranslator_UnknownLocaleFallsBack(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"greeting": "Hello"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	tr := bundle.Translator("xx")
	if tr.Locale() != "en" {
		t.Errorf("Translator(xx) locale = %q, want %q", tr.Locale(), "en")
	}
	if got := tr.T("greeting"); got != "Hello" {
		t.Errorf("fallback T(greeting) = %q, want %q", got, "Hello")
	}
}

func TestMiddleware(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "en.json", `{"msg": "Hello"}`)
	writeLocaleFile(t, dir, "de.json", `{"msg": "Hallo"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// Handler that uses the translator from context.
	var gotMsg string
	handler := Middleware(bundle)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := TFromCtx(r.Context())
		gotMsg = tr.T("msg")
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		acceptLang string
		wantMsg    string
	}{
		{"english default", "", "Hello"},
		{"german header", "de", "Hallo"},
		{"unknown falls back", "fr", "Hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.acceptLang != "" {
				req.Header.Set("Accept-Language", tt.acceptLang)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotMsg != tt.wantMsg {
				t.Errorf("T(msg) = %q, want %q", gotMsg, tt.wantMsg)
			}
		})
	}
}

func TestBundleFallback_NoEnglish(t *testing.T) {
	dir := t.TempDir()
	writeLocaleFile(t, dir, "de.json", `{"greeting": "Hallo"}`)
	writeLocaleFile(t, dir, "fr.json", `{"greeting": "Bonjour"}`)

	bundle, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// Without "en", fallback should be the first locale alphabetically ("de").
	if got := bundle.Fallback(); got != "de" {
		t.Errorf("Fallback() = %q, want %q", got, "de")
	}
}

// writeLocaleFile is a test helper that creates a locale JSON file.
func writeLocaleFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
