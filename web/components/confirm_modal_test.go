package components

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// confirmModalHTML renders ConfirmModal under the given locale using the real
// embedded locale bundle, so the fr/ja assertions exercise the shipped JSON
// (including the English fallback for keys a partial locale omits).
func confirmModalHTML(t *testing.T, locale string) string {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator(locale))
	var buf bytes.Buffer
	if err := ConfirmModal().Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestConfirmModal_LocalizedChrome pins the modal's visible strings. The modal
// is mounted once in the layout and serves every hx-confirm, so en must stay
// exactly as it was while fr and ja show their own words and none of the
// English ones (#2678).
func TestConfirmModal_LocalizedChrome(t *testing.T) {
	tests := []struct {
		locale string
		want   []string
		absent []string
	}{
		{"en", []string{">Confirm<", ">Cancel<", "Don&#39;t ask again"}, nil},
		{"fr", []string{">Confirmer<", ">Annuler<", "Ne plus demander"},
			[]string{">Confirm<", ">Cancel<", "ask again"}},
		{"ja", []string{">確認<", ">キャンセル<", "今後は確認しない"},
			[]string{">Confirm<", ">Cancel<", "ask again"}},
	}
	for _, tt := range tests {
		t.Run(tt.locale, func(t *testing.T) {
			// Whitespace between tags varies; compare with it collapsed.
			html := strings.Join(strings.Fields(confirmModalHTML(t, tt.locale)), " ")
			html = strings.ReplaceAll(html, "> ", ">")
			html = strings.ReplaceAll(html, " <", "<")
			// The page is <html lang="en">: the nodes the template translates
			// carry the render locale; the root and the caller-filled message
			// must NOT (caller text is mostly English, JS tags it per call).
			for _, id := range []string{"confirm-modal-title", "confirm-modal-cancel", "confirm-modal-accept", "confirm-modal-remember-wrapper"} {
				if want := `id="` + id + `" lang="` + tt.locale + `"`; !strings.Contains(html, want) {
					t.Errorf("%s render missing %s", tt.locale, want)
				}
			}
			for _, id := range []string{"confirm-modal", "confirm-modal-message"} {
				if strings.Contains(html, `id="`+id+`" lang=`) || strings.Contains(html, `lang="`+tt.locale+`" id="`+id+`"`) {
					t.Errorf("%s render must not tag #%s with a language", tt.locale, id)
				}
			}
			for _, w := range tt.want {
				if !strings.Contains(html, w) {
					t.Errorf("%s render missing %q", tt.locale, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(html, a) {
					t.Errorf("%s render still contains English %q", tt.locale, a)
				}
			}
		})
	}
}

// The modal title is an h2: the modal opens over pages whose last heading is an
// h1 (or h2/h3), and an h3 there skips a level (axe heading-order).
func TestConfirmModal_TitleIsH2(t *testing.T) {
	html := confirmModalHTML(t, "en")
	if !strings.Contains(html, `<h2 id="confirm-modal-title"`) || strings.Contains(html, "<h3") {
		t.Errorf("confirm modal title must be an h2 (and no h3): %s", html)
	}
}
