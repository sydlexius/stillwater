package artist

// dupkey.go -- normalized identity key for near-duplicate artist detection.
//
// The key produced by NormalizeIdentityKey must collapse name variants that a
// human reads as identical but that arrive from different tools with different
// byte representations:
//
//   - NFC vs NFD of accented characters (common on macOS/APFS vs provider APIs).
//   - Apostrophe / quote / dash variants (U+2019 vs U+0027 is the observed live case).
//   - Whitespace: leading/trailing, doubled, non-breaking (U+00A0).
//   - Case differences.
//   - Leading/trailing articles ("The Cure" / "Cure, The" / "Cure").
//   - Punctuation-replaced filesystem characters: "AC/DC" -> "AC_DC" vs "ACDC".
//
// Unicode normalization ALONE is not sufficient: NFKC does not map U+2019
// (RIGHT SINGLE QUOTATION MARK) to U+0027 (APOSTROPHE), so an artist name
// like Larkfield's Reach can split into two records depending on which
// apostrophe form a given tool emits.  This is NOT caught by normalization
// alone.  The explicit punctuation fold at step 3 is therefore load-bearing.

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// caseFold is the Unicode case-fold transformer used in step 5.
// cases.Fold() with no options applies locale-independent Unicode case folding
// (tr/tc/sc fields from CaseFolding.txt).  Locale-independent is correct here
// because artist names come from arbitrary locales and we want consistent
// folding regardless of the server's locale setting.
var caseFold = cases.Fold()

// commonArticles are the English articles stripped from the beginning or
// rewritten from the ", The" suffix form.  Lower-cased because the key
// has already been case-folded before the article strip step.
var commonArticles = []string{"the ", "a ", "an "}

// NormalizeIdentityKey returns a compact string key that is equal for artist
// names a human would read as the same artist.  Two artists with the same key
// are candidates for the near-duplicate group.  Empty input returns "".
//
// The pipeline is ordered so each step operates on the output of the previous:
//
//  1. NFKC normalization -- folds fullwidth forms (U+FF01...), ligatures (U+FB01
//     fi ligature -> "fi"), compatibility decomposition + canonical composition.
//     This is the widest single Unicode normalization pass.
//
//  2. Strip Unicode format / default-ignorable characters (category Cf) -- zero-
//     width space (U+200B), zero-width joiner (U+200D), BOM (U+FEFF), bidi marks
//     (U+200E, U+200F), etc.  These are invisible in rendered text but make byte
//     comparison fail.  Running this AFTER NFKC is correct: NFKC can produce Cf
//     characters from some inputs, and we want to strip the post-normalized form.
//
//  3. Explicit punctuation fold -- NFKC does NOT map:
//     U+2019 (RIGHT SINGLE QUOTATION MARK) -> U+0027 (APOSTROPHE)
//     U+2010..U+2015 (various dashes) -> U+002D (HYPHEN-MINUS)
//     U+201C/U+201D (curly double quotes) -> U+0022 (QUOTATION MARK)
//     These are the characters that cause the "Larkfield's Reach" split above.
//     We fold the whole family in each class to a single ASCII representative.
//
//  4. Whitespace collapse -- replace every run of Unicode whitespace (including
//     U+00A0 NON-BREAKING SPACE and other Unicode Zs characters) with a single
//     ASCII space (U+0020), then trim leading and trailing spaces.  Running after
//     punctuation fold ensures that a dash-to-space fold does not produce doubled
//     spaces that survive into the key.
//
//  5. Unicode case-fold (cases.Fold, not strings.ToLower) -- locale-independent
//     case equivalence.  Correct for non-ASCII characters such as the German
//     sharp-S (U+00DF) -> "ss".
//
//  6. Article strip -- always strip a leading article ("the ", "a ", "an ") and
//     normalize a trailing ", the" / ", a" / ", an" suffix back to the bare name.
//     Article mode is always strip for duplicate key purposes: the goal is to
//     collapse "The Cure", "Cure, The", and "Cure" to the same key.
//
//  7. Separator fold -- collapse every run of hyphen-minus (U+002D), underscore
//     (U+005F), and space (U+0020) into a single space.  This collapses the
//     filesystem-reserved-character substitutions:
//     "AC/DC" -> directory stored as "AC_DC" or "ACDC" or "AC-DC"
//     All three share the same key after this step.
func NormalizeIdentityKey(name string) string {
	if name == "" {
		return ""
	}

	// Step 1: NFKC normalization.
	name = norm.NFKC.String(name)

	// Step 2: strip Unicode format (Cf) characters.
	// Cf includes zero-width space, BOM, bidi marks, soft hyphen, etc.
	name = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1 // drop
		}
		return r
	}, name)

	// Step 3: explicit punctuation fold.
	// Each family is folded to a single ASCII representative that survives
	// the later case-fold and separator-fold steps unchanged.
	name = foldPunctuation(name)

	// Step 4: whitespace collapse.
	// unicode.IsSpace matches U+0020 (space), U+00A0 (NBSP), U+1680,
	// U+2000-U+200A, U+2028, U+2029, U+202F, U+205F, U+3000, and the
	// common ASCII whitespace set (\t \n \r \f \v).
	// Replace each whitespace run with a single space, then trim.
	var sb strings.Builder
	inSpace := false
	for _, r := range name {
		if unicode.IsSpace(r) {
			if !inSpace {
				sb.WriteByte(' ')
				inSpace = true
			}
			// else skip additional whitespace in the run
		} else {
			sb.WriteRune(r)
			inSpace = false
		}
	}
	name = strings.TrimSpace(sb.String())

	if name == "" {
		return ""
	}

	// Step 5: Unicode case-fold.
	// cases.Fold with language.Und applies UNICODE CASE FOLDING (tr/tc/sc
	// fields in CaseFolding.txt) rather than locale-specific title/upper/
	// lower.  This is correct for artist names from arbitrary locales.
	name = caseFold.String(name)

	// Step 6: article strip (always-strip mode for duplicate key purposes).
	// We handle two article positions:
	//   Prefix: "the cure"   -> "cure"
	//   Suffix: "cure, the"  -> "cure"   (suffix form used in sort_name)
	//
	// The case-fold in step 5 lower-cased the name, so the article list is
	// already lower-cased (commonArticles).  We strip at most one article.

	// Suffix form: "cure, the" -> "cure"
	for _, art := range commonArticles {
		// art includes a trailing space ("the "); strip that for the suffix check.
		bare := strings.TrimSuffix(art, " ") // "the"
		suffix := ", " + bare                // ", the"
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}

	// Prefix form: "the cure" -> "cure"
	for _, art := range commonArticles {
		if strings.HasPrefix(name, art) && len(name) > len(art) {
			name = name[len(art):]
			break
		}
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	// Step 7: separator fold.
	// Collapse runs of hyphen-minus (U+002D), underscore (U+005F), and ASCII
	// space into a single space.  This merges "AC_DC", "AC-DC", "AC DC" into
	// the same key, catching the common class of filesystem-reserved-character
	// substitutions that a bare name-key cannot otherwise recover.
	var out strings.Builder
	out.Grow(len(name))
	inSep := false
	for _, r := range name {
		if r == '-' || r == '_' || r == ' ' {
			if !inSep {
				out.WriteByte(' ') // normalize all separator runs to a single space
				inSep = true
			}
		} else {
			out.WriteRune(r)
			inSep = false
		}
	}
	return strings.TrimSpace(out.String())
}

// foldPunctuation replaces each rune in name with its ASCII representative
// according to the character families defined in the issue spec.  Runes not in
// any fold family are passed through unchanged.
func foldPunctuation(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		// Apostrophe family -> U+0027 APOSTROPHE
		// Includes the observed U+2019 (RIGHT SINGLE QUOTATION MARK, used by
		// MusicBrainz canonical names) that NFKC does NOT normalize.
		case '’', '‘', 'ʼ', '`', '´', '′', '＇': // FULLWIDTH APOSTROPHE
			return '\'' // U+0027

		// Dash / hyphen family -> U+002D HYPHEN-MINUS
		case '‐', '‑', '‒', '–', '—', '―', '−', '﹘', '﹣', '－': // FULLWIDTH HYPHEN-MINUS
			return '-' // U+002D

		// Double-quote family -> U+0022 QUOTATION MARK
		case '“', '”', '«', '»': // RIGHT-POINTING DOUBLE ANGLE QUOTATION MARK
			return '"' // U+0022
		}
		return r
	}, name)
}
