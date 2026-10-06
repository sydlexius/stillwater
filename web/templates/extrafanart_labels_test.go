package templates

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// A known code gets its label. An unknown code, of every kind, renders that
// kind's generic label (never the bare key) and logs once per distinct code
// however many rows carry it. Not parallel: it swaps the process default
// logger, and it removes its keys from the process-global seen map so that
// -count=2 starts clean.
func TestExtraFanartLabel_UnknownCodeIsGenericAndLoggedOncePerCode(t *testing.T) {
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator("en"))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if got := extraFanartLabel(ctx, "outcome", "planned"); got != "Will Move" {
		t.Errorf("known code: got %q", got)
	}
	generic := map[string]string{"outcome": "Unrecognized Outcome", "reason": "unrecognized reason", "status": "Unrecognized Status"}
	for kind, want := range generic {
		for _, code := range []string{"zz_test_a", "zz_test_a", "zz_test_a", "zz_test_b"} {
			key := "extrafanart_migration." + kind + "_" + code
			t.Cleanup(func() { extraFanartUnknownSeen.Delete(key) })
			if got := extraFanartLabel(ctx, kind, code); got != want {
				t.Fatalf("%s %s: got %q, want the generic label %q", kind, code, got, want)
			}
		}
	}
	// Two distinct codes per kind, three kinds: six warnings, however many calls.
	if n := strings.Count(buf.String(), "no label for code"); n != 6 {
		t.Errorf("want 6 warnings (2 distinct codes x 3 kinds), got %d:\n%s", n, buf.String())
	}
}
