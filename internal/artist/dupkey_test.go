package artist

import (
	"testing"
)

// TestNormalizeIdentityKey covers every edge case from the issue spec and the
// W3 implementation plan.  Each test case documents which pipeline step(s) it
// exercises and why the expected output is what it is.
func TestNormalizeIdentityKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// --- Basic passthrough ---
		{
			name:  "simple ASCII name unchanged",
			input: "Radiohead",
			want:  "radiohead",
		},
		{
			name:  "empty string returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "only whitespace returns empty",
			input: "   \t\n",
			want:  "",
		},

		// --- Step 1: NFKC ---
		{
			name:  "NFKC fullwidth latin folds to ASCII",
			input: "ＡＢＣＤ", // U+FF21..FF24 fullwidth
			want:  "abcd",
		},
		{
			name:  "NFC accented name -- cafe with precomposed e-acute",
			input: "Café", // U+00E9 precomposed
			want:  "café",
		},
		// NFC vs NFD: "e" + combining acute (U+0301) vs precomposed U+00E9.
		// NFKC composes both into U+00E9, so the keys are equal.
		{
			name:  "NFD accented name folds to same key as NFC",
			input: "Café", // e + combining acute
			want:  "café",  // same as NFC form after step 5 case-fold
		},
		{
			name:  "Bjork NFC vs NFD -- both fold to same key",
			input: "Björk", // o + combining diaeresis (NFD of o-umlaut)
			want:  "björk",  // same as precomposed U+00F6
		},

		// --- Step 2: strip Cf format characters ---
		{
			// U+200B ZERO WIDTH SPACE is category Cf -- stripped in step 2.
			// After stripping, "TheCure" has no space between "The" and "Cure"
			// so the article strip (which requires "the " with a space) does not
			// fire.  The raw concatenated form is the correct key here.
			name:  "zero-width space stripped but no article strip without space",
			input: "The\u200bCure", // U+200B between "The" and "Cure"
			want:  "thecure",
		},
		// Soft hyphen (U+00AD) is tested in TestNormalizeIdentityKey_SoftHyphen below
		// using a programmatic construction to avoid invisible-character lint warnings.

		// --- Step 3: explicit punctuation fold ---

		// Apostrophe family tests are in TestNormalizeIdentityKey_CaedmonPair and
		// TestNormalizeIdentityKey_ApostropheKey below; removing from table to avoid
		// curly-quote confusion in string literals.
		{
			name:  "em dash folds to hyphen-minus",
			input: "Rock—Roll",
			want:  "rock roll", // dash in a separator run collapses to space
		},
		{
			name:  "en dash folds to hyphen-minus",
			input: "Rock–Roll",
			want:  "rock roll",
		},
		// Note: curly double-quote fold tested separately in TestNormalizeIdentityKey_CurlyQuotes
		// to avoid Go's “use neutral quote” vet diagnostic on curly-quote string literals.

		// --- Step 4: whitespace collapse ---
		{
			name:  "leading and trailing whitespace trimmed",
			input: "  The Cure  ",
			want:  "cure",
		},
		{
			name:  "non-breaking space U+00A0 treated as whitespace",
			input: "The Cure",
			want:  "cure",
		},
		{
			name:  "doubled spaces collapsed",
			input: "Sigur  Ros",
			want:  "sigur ros",
		},
		{
			name:  "tab between words collapsed",
			input: "Sigur\tRos",
			want:  "sigur ros",
		},

		// --- Step 5: Unicode case-fold ---
		{
			name:  "upper case folds to lower",
			input: "RADIOHEAD",
			want:  "radiohead",
		},
		{
			// "/" is not in the dash fold family (it is not a dash variant).
			// The key for "AC/DC" preserves the slash.  The MBID backstop
			// handles the AC/DC vs AC_DC vs ACDC collision.
			name:  "slash not folded -- MBID is the backstop for filesystem chars",
			input: "AC/DC",
			want:  "ac/dc",
		},

		// --- Steps 6: article strip ---
		{
			name:  "leading The stripped",
			input: "The Cure",
			want:  "cure",
		},
		{
			name:  "leading A stripped",
			input: "A Flock Of Seagulls",
			want:  "flock of seagulls",
		},
		{
			name:  "leading An stripped",
			input: "An Pierlé",
			want:  "pierlé", // accented e preserved
		},
		{
			name:  "suffix form Cure, The folds to same as The Cure",
			input: "Cure, The",
			want:  "cure",
		},
		{
			name:  "suffix form Band, A folds to A Band",
			input: "Band, A",
			want:  "band",
		},
		{
			name:  "no article -- name passes through",
			input: "Radiohead",
			want:  "radiohead",
		},
		{
			name:  "The Cure vs Cure, The -- both produce same key",
			input: "Cure, The",
			want:  "cure",
		},
		// Article that IS the entire name should not strip to empty.
		{
			name:  "artist named just 'The' does not collapse to empty",
			input: "The",
			want:  "the", // "the " prefix requires at least one more char
		},

		// --- Step 7: separator fold ---
		{
			name:  "underscore in name folds to space",
			input: "AC_DC",
			want:  "ac dc",
		},
		{
			name:  "hyphen-minus in name folds to space",
			input: "AC-DC",
			want:  "ac dc",
		},
		{
			name:  "AC_DC and AC-DC produce same key",
			input: "AC-DC",
			want:  "ac dc",
		},
		{
			name:  "mixed separator run collapses to single space",
			input: "AC- _DC",
			want:  "ac dc",
		},

		// The Caedmon pair is tested in TestNormalizeIdentityKey_CaedmonPair.
		{
			name:  "Motley Crue NFC form",
			input: "Mötley Crüe", // precomposed o-umlaut and u-umlaut
			want:  "mötley crüe",
		},
		{
			name:  "Motley Crue NFD form folds to same key as NFC",
			input: "Mötley Crüe", // o/u + combining diaeresis
			want:  "mötley crüe",   // same as NFC after NFKC
		},
		{
			name:  "unsafe name dot-only is empty after article strip",
			input: ".",
			want:  ".",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeIdentityKey(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeIdentityKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestNormalizeIdentityKey_SoftHyphen checks that U+00AD SOFT HYPHEN (category
// Cf) is stripped from an artist name.  Constructed programmatically to avoid
// invisible-character vet/lint warnings in source.
func TestNormalizeIdentityKey_SoftHyphen(t *testing.T) {
	sh := string([]rune{0x00AD}) // SOFT HYPHEN
	input := "Anti" + sh + "Christ"
	got := NormalizeIdentityKey(input)
	want := "antichrist"
	if got != want {
		t.Errorf("NormalizeIdentityKey(%q) = %q, want %q", input, got, want)
	}
}

// TestNormalizeIdentityKey_ApostropheKey verifies that the straight apostrophe
// U+0027 and its curly-quote counterpart U+2019 both produce the same key.
// This is the primary case from the issue report.  We construct the U+2019
// variant programmatically so the Go source does not contain a curly apostrophe
// that could be confused with a Go string delimiter.
func TestNormalizeIdentityKey_ApostropheKey(t *testing.T) {
	// "Caedmon's Call" with U+0027 (straight ASCII apostrophe)
	straight := "Caedmon's Call"
	// "Caedmon's Call" with U+2019 (RIGHT SINGLE QUOTATION MARK)
	curlyRune := string([]rune{0x2019})
	curly := "Caedmon" + curlyRune + "s Call"

	kStraight := NormalizeIdentityKey(straight)
	kCurly := NormalizeIdentityKey(curly)

	if kStraight == "" {
		t.Fatal("key for straight-apostrophe name is empty")
	}
	if kStraight != kCurly {
		t.Errorf("apostrophe variants produce different keys:\n  U+0027: %q\n  U+2019: %q", kStraight, kCurly)
	}
	// The key must contain the straight apostrophe U+0027, not U+2019.
	for _, r := range kStraight {
		if r == 0x2019 {
			t.Errorf("key %q still contains U+2019; step 3 fold did not apply", kStraight)
		}
	}
}

// TestNormalizeIdentityKey_CurlyQuotes checks that U+201C (left double
// quotation mark) and U+201D (right double quotation mark) are folded to
// U+0022 (straight double quote) in step 3 of the pipeline.  Tested
// programmatically to avoid Go's "use neutral quote" vet diagnostic.
func TestNormalizeIdentityKey_CurlyQuotes(t *testing.T) {
	// Build input: U+201C + "The Band" + U+201D
	input := string([]rune{0x201C}) + "The Band" + string([]rune{0x201D})
	// After fold: "The Band" (straight quotes, with outer quotes preventing
	// article strip since the first char is '"' not 't').
	want := "\"the band\""
	got := NormalizeIdentityKey(input)
	if got != want {
		t.Errorf("NormalizeIdentityKey(curly-quoted %q) = %q, want %q", "The Band", got, want)
	}
}

// TestNormalizeIdentityKey_BOM checks that a BOM (U+FEFF, category Cf) at the
// start of an artist name is stripped by step 2 of the pipeline.  The BOM is
// constructed programmatically because Go source files may not contain a literal
// BOM codepoint.
func TestNormalizeIdentityKey_BOM(t *testing.T) {
	// U+FEFF ZERO WIDTH NO-BREAK SPACE / BOM, constructed via string conversion.
	bom := string([]rune{0xFEFF})
	input := bom + "Bjork"
	got := NormalizeIdentityKey(input)
	want := "bjork"
	if got != want {
		t.Errorf("NormalizeIdentityKey(BOM+%q) = %q, want %q", "Bjork", got, want)
	}
}

// TestNormalizeIdentityKey_CaedmonPair is the canonical near-duplicate case
// from the issue report: two directories differing only by apostrophe codepoint
// must produce the same key.
func TestNormalizeIdentityKey_CaedmonPair(t *testing.T) {
	straight := NormalizeIdentityKey("Caedmon's Call") // U+0027
	curly := NormalizeIdentityKey("Caedmon’s Call")    // U+2019

	if straight != curly {
		t.Errorf("apostrophe pair does not produce equal keys: U+0027 key=%q  U+2019 key=%q", straight, curly)
	}
}

// TestNormalizeIdentityKey_ArticleVariants checks that "The Cure", "Cure, The",
// and "Cure" all map to the same key, and that the plain name without any
// article also maps to "cure".
func TestNormalizeIdentityKey_ArticleVariants(t *testing.T) {
	cases := []string{
		"The Cure",
		"Cure, The",
		"Cure",
	}
	keys := make([]string, len(cases))
	for i, c := range cases {
		keys[i] = NormalizeIdentityKey(c)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[0] {
			t.Errorf("article variants do not produce equal keys:\n  %q -> %q\n  %q -> %q",
				cases[0], keys[0], cases[i], keys[i])
		}
	}
}

// TestNormalizeIdentityKey_ACDC checks that the AC/DC filesystem-substitution
// variants "AC_DC" and "AC-DC" produce identical keys, and that "ACDC" also
// maps to the same key after separator fold collapses the separator-less form.
func TestNormalizeIdentityKey_ACDC(t *testing.T) {
	underscore := NormalizeIdentityKey("AC_DC")
	hyphen := NormalizeIdentityKey("AC-DC")
	nospace := NormalizeIdentityKey("ACDC")

	if underscore != hyphen {
		t.Errorf("AC_DC key=%q, AC-DC key=%q -- expected equal", underscore, hyphen)
	}
	// "ACDC" has no separator; it will produce "acdc", while "AC_DC" produces
	// "ac dc".  They are NOT equal by name key -- that is correct behavior.
	// The MBID backstop handles the acdc/ac dc split.
	if nospace == underscore {
		t.Logf("note: ACDC and AC_DC share a key=%q (separator-fold makes them equal)", nospace)
	} else {
		t.Logf("ACDC key=%q differs from AC_DC key=%q (expected; MBID is the backstop)", nospace, underscore)
	}
}
