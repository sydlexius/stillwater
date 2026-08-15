package artist

import (
	"errors"
	"testing"
)

// A PUNCTUATION-ONLY name must be refused by ValidateFieldUpdate (#3037).
//
// THE DEFECT. ValidateFieldUpdate refused only strings.TrimSpace(value) == "".
// NormalizeIdentityKey folds dashes, underscores and Unicode format (Cf)
// characters out entirely, so a name of "-", "_", an em-dash or a zero-width
// space passed that check and still produced an EMPTY identity key. Both
// mechanisms that exist to catch a duplicate then skip an empty key on
// purpose: the collision guard treats it as "not a collision"
// (name_collision.go) and the duplicates report's unionByNameKey drops it
// (duplicates.go). Two artists could therefore both be named "-" and be
// invisible to the guard AND to the report -- a duplicate nothing in the
// product can find, which is strictly worse than the #2730 duplicate the guard
// was built for.
//
// READ THIS BEFORE EDITING. Every refusal assertion here is paired with a
// POSITIVE CONTROL through the SAME call, because a validator that refused
// everything would satisfy a refusal-only test. And the boundary the fix must
// not overreach gets its own test: names in every major non-Latin script have
// to stay ACCEPTED. A validator that refused those would be far worse than the
// bug it closes.

// punctuationOnlyNames are inputs that survive TrimSpace but normalize to an
// empty identity key. Each is a real shape an operator or an import can
// produce, not a synthetic edge case: a placeholder dash, a filesystem-safe
// underscore substitution, a typographic dash pasted from a web page, and a
// zero-width space smuggled in by a copy-paste.
var punctuationOnlyNames = []string{
	"-",
	"_",
	"--",
	"- _ -",
	"—",            // em-dash
	"\u200b",       // zero-width space
	" \u200b \t -", // mixed spacing, Cf and dash
}

// TestNormalizeIdentityKey_PunctuationOnlyIsEmpty asserts the PRECONDITION the
// rest of this file depends on. Without it, a normalizer that stopped folding
// dashes would make every refusal below pass for the wrong reason.
func TestNormalizeIdentityKey_PunctuationOnlyIsEmpty(t *testing.T) {
	t.Parallel()
	for _, name := range punctuationOnlyNames {
		if got := NormalizeIdentityKey(name); got != "" {
			t.Errorf("PRECONDITION FAILED: NormalizeIdentityKey(%q) = %q, want empty; "+
				"this input no longer produces an empty identity key, so the refusal "+
				"tests below no longer exercise the defect they were written for", name, got)
		}
	}
}

// TestValidateFieldUpdate_AcceptsNonLatinNames is the POSITIVE CONTROL for the
// whole rule, and the guard against the over-correction: refusing a legitimate
// non-Latin name would be a far worse defect than the punctuation hole.
//
// It passes with and without the fix, and that is correct -- it is a boundary
// guard, not a regression test. If it ever fails, the normalizer (dupkey.go),
// not the validator, is what changed.
func TestValidateFieldUpdate_AcceptsNonLatinNames(t *testing.T) {
	t.Parallel()
	names := []string{
		"坂本龍一",                                        // CJK
		"Пугачёва",                                    // Cyrillic
		"Βαγγέλης",                                    // Greek
		"אריק",                                        // Hebrew
		"فيروز",                                       // Arabic
		"คาราบาว",                                     // Thai
		"방탄소년단",                                       // Hangul
		"Björk", "Sigur Rós", "Ásgeir", "Mötley Crüe", // accented Latin
		"一",          // a single CJK ideograph: shortest possible non-Latin name
		"ＡＢＣ",        // fullwidth Latin, folded by NFKC rather than dropped
		"!!!", "†††", // real band names made entirely of punctuation the fold KEEPS
		"+/-", "{+/-}", // real band names whose dashes fold but whose slashes do not
		"AC/DC", "P!nk", // punctuation-heavy Latin names
		"65daysofstatic", // leading digits
	}
	for _, name := range names {
		// PRECONDITION: the normalizer produces an identity for this name.
		// Asserted first so an accept below cannot pass because the validator
		// stopped checking anything at all.
		if NormalizeIdentityKey(name) == "" {
			t.Errorf("NormalizeIdentityKey(%q) is empty: a real-world name folds to NOTHING. "+
				"This is a normalizer defect, not a validation one: such an artist is "+
				"invisible to the collision guard and to the duplicates report.", name)
			continue
		}
		if err := ValidateFieldUpdate(string(FieldArtistName), name); err != nil {
			t.Errorf("ValidateFieldUpdate(name, %q) = %v, want accepted; the rule refuses "+
				"only names with NO identity key, never a script it does not recognize", name, err)
		}
	}
}

// TestValidateFieldUpdate_RefusesPunctuationOnlyName pins the rule at the one
// pre-write check every caller of ValidateFieldUpdate shares.
func TestValidateFieldUpdate_RefusesPunctuationOnlyName(t *testing.T) {
	t.Parallel()

	// POSITIVE CONTROL: an ordinary name still passes the same call.
	if err := ValidateFieldUpdate(string(FieldArtistName), "Harrowdene Ensemble"); err != nil {
		t.Fatalf("positive control FAILED: an ordinary name was refused (%v); "+
			"the refusal assertions below would pass vacuously", err)
	}

	for _, name := range punctuationOnlyNames {
		err := ValidateFieldUpdate(string(FieldArtistName), name)
		if err == nil {
			t.Errorf("ValidateFieldUpdate(name, %q) ACCEPTED; it normalizes to an empty "+
				"identity key, so the collision guard skips its scan and the duplicates "+
				"report cannot group the row", name)
			continue
		}
		if !errors.Is(err, ErrInvalidFieldValue) {
			t.Errorf("ValidateFieldUpdate(name, %q) err = %v, want one matching "+
				"ErrInvalidFieldValue so a caller can classify the refusal without "+
				"type-asserting; an unclassifiable refusal reads as a 500, not a 400", name, err)
		}
		// The Reason reaches an HTTP 400 body verbatim (handleFieldUpdate passes
		// Error() straight through), so it is pinned below character-for-character
		// and SPELLED OUT, never read from the production symbol -- sharing that
		// symbol would accept any rewording, an input echo included.
		var ve *FieldValidationError
		if !errors.As(err, &ve) {
			t.Errorf("ValidateFieldUpdate(name, %q) err is not a *FieldValidationError, "+
				"so no operator-facing Reason reaches the 400 body", name)
			continue
		}
		const want = "name cannot be only dashes, underscores, spacing, or invisible formatting characters"
		if ve.Reason != want {
			t.Errorf("ValidateFieldUpdate(name, %q) Reason = %q, want %q", name, ve.Reason, want)
		}
	}
}

// TestValidateFieldUpdate_SortNameIsNotSubjectToTheIdentityRule pins the SCOPE
// decision. sort_name feeds display ordering, never identity: no identity key,
// collision scan or duplicate grouping reads it. Clearing it back to empty so
// the artist sorts by name is a legitimate operator act, and applying the name
// rule to it would block that.
func TestValidateFieldUpdate_SortNameIsNotSubjectToTheIdentityRule(t *testing.T) {
	t.Parallel()
	// PRECONDITION: these inputs ARE refused for "name". Without this the test
	// would pass against a validator that refuses nothing at all.
	if err := ValidateFieldUpdate(string(FieldArtistName), "-"); err == nil {
		t.Fatal(`PRECONDITION FAILED: "-" is accepted for name, so accepting it for ` +
			"sort_name proves nothing about scope")
	}
	for _, value := range append([]string{""}, punctuationOnlyNames...) {
		if err := ValidateFieldUpdate(string(FieldSortName), value); err != nil {
			t.Errorf("ValidateFieldUpdate(sort_name, %q) = %v, want accepted: sort_name is a "+
				"display-order field and clearing it is a legitimate operator act", value, err)
		}
	}
}
