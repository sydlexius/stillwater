#!/usr/bin/env bash
# check-muted-text-pair.sh -- fail when a .templ file under web/ or a hand-written
# .js file under web/static/js puts the failing muted-text color pair on ONE
# element's class list (#3474).
#
# THE BUG THIS EXISTS FOR. The pair "gray-400 in light, gray-500 in dark" (the two
# Tailwind classes named by $LIGHT and $DARK below) paints at roughly 2.5 to 3.1:1
# on the app's translucent glass cards, against the 4.5:1 WCAG AA needs. axe
# reports text over a translucent surface as "incomplete" rather than a
# violation, so the a11y tier cannot catch it; the only guard is to refuse the
# pair at the source. The replacement is gray-600 in light and gray-400 in dark.
#
# THIS FILE DELIBERATELY NEVER WRITES THE TWO CLASS NAMES IN ONE PIECE. Tailwind
# v4 scans the whole project, shell scripts included, and harvests anything that
# looks like a class name, so a literal class name in this file would be minted
# into the shipped stylesheet. The names are assembled from fragments instead
# (see check-css-comments.sh for the incident that taught this).
#
# WHAT IT SCANS. Every .templ file under web/, and every .js file under
# web/static/js EXCEPT minified vendor bundles (*.min.js: cropper, chart, htmx,
# Sortable, driver, scalar). Those are third-party builds that this repo does not
# edit, and they hold no first-party class lists; everything else in that
# directory (and its settings/ and artist-detail/ subdirectories) is
# hand-written and is scanned.
#
# WHAT IT MATCHES (positive and precise, not a whole-line grep). The two classes
# must be whole tokens inside the SAME class list, which is one of:
#   1. one quoted string: class="a b c" (also across lines), a Go string passed to
#      templ.KV("...", cond), a JS string such as el.className = '...'. The
#      string is found two ways and EITHER counts: (a) the nearest quote of any
#      kind left and right of a token (a quote elsewhere on the line does not
#      matter, and an apostrophe in prose cannot hide a class string on its
#      line); (b) a left-to-right quote-aware pass in which a different quote
#      character INSIDE an open double-quoted or backtick string (an apostrophe,
#      or content-['x'] in a class) neither closes nor opens anything. (a) alone
#      missed a pair that followed such an inner quote; (b) alone would be
#      hidden by a prose apostrophe, so they are unioned;
#   2. one templ class expression: class={ "a", templ.KV("b", ok), ... }, where
#      the tokens may sit in different string literals of the same braces;
#   3. the argument list of ONE call that applies EVERY string argument as a
#      class: classList.add(...) or templ.Classes(...), where the two classes are
#      separate string literals, e.g. classList.add('a', 'b'). classList.toggle
#      and classList.replace are NOT in this group (see below).
# Either order, and with any other classes between them, is caught. Reported as
# file:line (the line of the first of the two tokens).
#
# WHAT IT DOES NOT CATCH (by design or limitation):
#   - the two classes on DIFFERENT elements, even on one line (correct: that is
#     not the failing pair), or added by DIFFERENT classList/templ.Classes calls
#     (two calls are not one class list at the point of writing; review by hand);
#   - the two classes as separate literals of classList.toggle(token, force) (the
#     second argument is a boolean force flag, never applied as a class) or of
#     classList.replace(old, new) (old is removed, so the pair never coexists);
#     classList.remove is never matched. A single literal holding BOTH classes
#     passed to toggle or replace IS still reported, by the string finders (1);
#   - variants such as hover: or dark:hover: forms, the Tailwind important
#     marker (a leading or trailing bang on a class), and opacity forms like
#     <class>/50 (different tokens, different painted color; review by hand);
#   - a string with an escaped quote inside it (neither string finder honors a
#     backslash escape);
#   - a pair hidden by BOTH finders at once: an unbalanced double quote in prose
#     (which flips the quote-aware pass for the rest of the file) combined with
#     a different-kind quote inside the class string before the tokens;
#   - the two classes in different JS literals joined by + (one class list at run
#     time, several strings in the source);
#   - a Tailwind @apply of the two utilities in a stylesheet (CSS is not scanned);
#   - a class list assembled at run time from separate variables or
#     concatenated pieces;
#   - files other than .templ under web/ and non-minified .js under
#     web/static/js (Go strings in internal/, CSS, inline <script> blocks ARE
#     inside .templ files and so ARE scanned, but Go-side class helpers are not);
#   - the same color reached through a different class (a hex value, a
#     different gray). This guards one known-bad pair, not contrast in general:
#     tests/a11y/muted-text-contrast.spec.js measures what is actually painted.
#
# Usage:
#   bash scripts/check-muted-text-pair.sh               scan web/ under the repo
#   bash scripts/check-muted-text-pair.sh --self-test   prove the matcher
# Exit: 0 clean, 1 pair found / self-test failed, 2 bad invocation or no python3.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "FAIL: check-muted-text-pair.sh needs python3" >&2
  exit 2
fi

MODE="scan"
case "${1:-}" in
  "") ;;
  --self-test) MODE="self-test" ;;
  *) echo "usage: check-muted-text-pair.sh [--self-test]" >&2; exit 2 ;;
esac

# The matcher is Python because the class list can span lines and sit inside
# brace-balanced templ expressions, which a line-oriented grep cannot express.
# MODE and REPO_ROOT reach it through the environment (never interpolated into
# the program text).
MODE="$MODE" REPO_ROOT="$REPO_ROOT" exec python3 -I - <<'PY'
import os
import re
import sys

# The two classes, assembled from fragments (see the header: no literal class
# name may appear in this file).
LIGHT = "text-gray-" + "400"
DARK = "dark:text-" + "gray-500"
QUOTES = "\"'`"

# A token is whole when the characters on both sides are whitespace, a quote, or
# the string edge: this rejects hover:/dark:hover: prefixes and /50 suffixes.
def token_re(tok):
    return re.compile(r"(?<![^\s\"'`])" + re.escape(tok) + r"(?![^\s\"'`])")

LIGHT_RE = token_re(LIGHT)
DARK_RE = token_re(DARK)

def line_of(text, index):
    return text.count("\n", 0, index) + 1

def nearest_quote_string(text, index):
    """Return the quoted string around text[index] by nearest quote, or None.

    Scans left to the nearest quote character of ANY kind, then right to the next
    one of the same kind. Fooled by a different-kind quote inside a double-quoted
    string (see quote_aware_spans), but immune to an unbalanced quote elsewhere.
    A single-quoted string must not cross a newline, because an apostrophe in
    prose would otherwise pair with a quote lines away. Double quotes and
    backticks may span lines (templ attributes, JS template literals).
    """
    left = max(text.rfind(q, 0, index) for q in QUOTES)
    if left < 0:
        return None
    quote = text[left]
    right = text.find(quote, index)
    if right < 0:
        return None
    segment = text[left + 1:right]
    if quote == "'" and "\n" in segment:
        return None
    return segment

def quote_aware_spans(text):
    """Return [(start, end)] of every string's content, found left to right.

    State machine: outside any string, the first quote character opens a string
    of that kind. Inside a string only its OWN quote character closes it, so an
    apostrophe or content-['x'] inside a double-quoted class string is ignored.
    A single-quoted string that reaches a newline is dropped (it was an
    apostrophe in prose, not a string), and scanning resumes after the newline.
    """
    spans = []
    quote, start = "", 0
    for i, c in enumerate(text):
        if quote:
            if c == quote:
                spans.append((start, i))
                quote = ""
            elif c == "\n" and quote == "'":
                quote = ""
        elif c in QUOTES:
            quote, start = c, i + 1
    return spans

def string_candidates(text, index, spans):
    """Yield each string that encloses text[index]: nearest-quote, then quote-aware."""
    seg = nearest_quote_string(text, index)
    if seg is not None:
        yield seg
    for start, end in spans:
        if start <= index < end:
            yield text[start:end]
            break

def class_expressions(text):
    """Yield (offset, string-literal text) for each class={ ... } expression.

    offset is where the first of the two classes sits inside the braces, so a
    pair that also matches as one string reports the same line once.
    """
    for m in re.finditer(r"\bclass=\{", text):
        depth, i = 1, m.end()
        while i < len(text) and depth:
            depth += {"{": 1, "}": -1}.get(text[i], 0)
            i += 1
        body = text[m.end():i - 1]
        literals = re.findall(r"\"[^\"]*\"|`[^`]*`", body)
        firsts = [h.start() for h in (LIGHT_RE.search(body), DARK_RE.search(body)) if h]
        yield m.end() + (min(firsts) if firsts else 0), " ".join(literals)

# ONLY calls that apply EVERY string argument as a class, so two separate
# literals really do land on one element together (DOMTokenList spec):
#   classList.add(a, b, ...)  adds every argument.
#   templ.Classes(a, b, ...)  merges every string argument into one class list.
# Deliberately excluded:
#   classList.toggle(token, force)  takes ONE class; the 2nd argument is a
#       boolean force flag, never applied as a class.
#   classList.replace(old, new)     REMOVES old and adds new, so the pair never
#       coexists after the call.
#   classList.remove(...)           removes classes; never creates the pair.
# A pair inside ONE string literal passed to any of these (toggle/replace
# included) is still reported, by the ordinary string finders in find_pairs.
CALL_RE = re.compile(r"\b(?:classList\.add|templ\.Classes)\(")

def call_expressions(text):
    """Yield (offset, joined string literals) for each class-applying call.

    Covers classList.add(...) and templ.Classes(...), where the
    two classes are separate string literals of ONE argument list. The argument
    list is found by balancing parentheses while skipping over string literals
    (so a paren inside a class string or a ternary does not end it early).
    """
    for m in CALL_RE.finditer(text):
        depth, i, quote = 1, m.end(), ""
        while i < len(text) and depth:
            c = text[i]
            if quote:
                if c == quote:
                    quote = ""
                elif c == "\\":
                    i += 1
            elif c in QUOTES:
                quote = c
            elif c == "(":
                depth += 1
            elif c == ")":
                depth -= 1
            i += 1
        body = text[m.end():i - 1]
        literals = re.findall(r"\"[^\"\n]*\"|'[^'\n]*'|`[^`]*`", body)
        firsts = [h.start() for h in (LIGHT_RE.search(body), DARK_RE.search(body)) if h]
        yield m.end() + (min(firsts) if firsts else 0), literals

def find_pairs(text):
    """Return the sorted line numbers where both classes share one class list."""
    lines = set()
    spans = quote_aware_spans(text)
    for m in LIGHT_RE.finditer(text):
        for seg in string_candidates(text, m.start(), spans):
            if LIGHT_RE.search(seg) and DARK_RE.search(seg):
                lines.add(line_of(text, m.start()))
    for offset, joined in class_expressions(text):
        if LIGHT_RE.search(joined) and DARK_RE.search(joined):
            lines.add(line_of(text, offset))
    for offset, literals in call_expressions(text):
        # Each literal is matched on its own AND joined, so 'a', 'b' (separate
        # strings) and 'a b' (one string) both count as one argument list.
        joined = " ".join(l[1:-1] for l in literals)
        if LIGHT_RE.search(joined) and DARK_RE.search(joined):
            lines.add(line_of(text, offset))
    return sorted(lines)

def self_test():
    L, D = LIGHT, DARK
    # (name, template text, expected number of reported lines)
    cases = [
        ("adjacent", '<p class="mt-1 %s %s">x</p>' % (L, D), 1),
        ("reversed", '<p class="%s %s mt-1">x</p>' % (D, L), 1),
        ("separated by other classes", '<p class="%s px-2 py-1 italic %s">x</p>' % (L, D), 1),
        ("pair in a templ.KV string", 'class={ "a", templ.KV("bg-gray-100 %s dark:bg-gray-700 %s", ok) }' % (L, D), 1),
        ("pair in a JS string", "el.className = 'px-3 %s %s italic';" % (L, D), 1),
        ("class list split across lines", '<p class="px-2\n  %s\n  %s">x</p>' % (L, D), 1),
        ("split across literals of one class expression", 'class={ "%s", templ.KV("%s", ok) }' % (L, D), 1),
        ("the allowed replacement pair", '<p class="text-gray-600 dark:text-gray-400">x</p>', 0),
        ("the two tokens on different elements of one line",
         '<p class="%s">a</p><p class="%s">b</p>' % (L, D), 0),
        ("light class with a hover variant of the dark class",
         '<p class="%s dark:hover:text-gray-500">x</p>' % L, 0),
        ("dark class with an opacity suffix", '<p class="%s %s/50">x</p>' % (L, D), 0),
        ("classList.add with the pair as two string literals",
         "el.classList.add('px-2', '%s', '%s');" % (L, D), 1),
        ("the pair in two different classList.toggle calls on one line",
         'el.classList.toggle("%s", on); el.classList.toggle("x", on, "%s");' % (D, L), 0),
        # toggle(token, force): the second argument is a boolean force flag, so a
        # second string is never applied as a class. Not a pair on one element.
        ("classList.toggle with the pair as two literals of one call (2nd arg is a force flag)",
         'el.classList.toggle("%s", "%s");' % (D, L), 0),
        # replace(old, new) REMOVES the first token and adds the second, so the
        # two never coexist on the element after the call.
        ("classList.replace with the pair as two literals (old is removed)",
         'el.classList.replace("%s", "%s");' % (L, D), 0),
        ("classList.toggle with both classes in ONE literal (string finders still report)",
         'el.classList.toggle("%s %s");' % (L, D), 1),
        ("className assignment of both classes in one double-quoted string",
         'el.className = "%s %s";' % (L, D), 1),
        ("classList.add with both classes in ONE literal (throws at run time, still reported)",
         'el.classList.add("%s %s");' % (L, D), 1),
        ("templ.Classes with the pair as two literals",
         '<p class={ templ.Classes("%s", "mt-1", "%s") }>x</p>' % (L, D), 1),
        ("the pair split across two DIFFERENT classList calls",
         "el.classList.add('%s');\nel.classList.add('%s');" % (L, D), 0),
        ("classList.add of one class only",
         "el.classList.add('px-2', '%s');" % L, 0),
        ("a paren inside a literal does not end the call early",
         "el.classList.add('a(b', '%s', '%s');" % (L, D), 1),
        ("an apostrophe in prose does not pair strings",
         '<p>it\'s</p>\n<p class="%s">a</p>\n<p class="%s">b</p>' % (L, D), 0),
        ("paired single quotes inside the class string, before the tokens",
         '<p class="before:content-[\'x\'] %s %s">x</p>' % (L, D), 1),
        ("a lone apostrophe inside the class string, before the tokens",
         '<p class="it\'s %s %s">x</p>' % (L, D), 1),
        ("paired single quotes between the tokens",
         '<p class="%s [&_a]:after:content-[\'x\'] %s">x</p>' % (L, D), 1),
        ("an apostrophe in a later attribute",
         '<p class="%s %s" title="it\'s here">x</p>' % (L, D), 1),
        ("an apostrophe in an earlier attribute",
         '<p title="it\'s" class="%s %s">x</p>' % (L, D), 1),
        ("an apostrophe in prose on the same line as the class string",
         '<p>it\'s <span class="%s %s">x</p>' % (L, D), 1),
        ("two attributes with a single-quoted JS fragment between them",
         '<p class="a %s" data-x=\'y\' class="%s b">x</p>' % (L, D), 0),
        ("two attributes each holding an apostrophe, on one line",
         '<p class="it\'s %s" title="it\'s"><i class="%s">x</i></p>' % (L, D), 0),
    ]
    failed = 0
    for name, text, want in cases:
        got = len(find_pairs(text))
        status = "ok" if got == want else "FAIL"
        if got != want:
            failed += 1
        print("self-test %-4s %s (reported %d line(s), want %d)" % (status, name, got, want))
    if failed:
        print("FAIL: %d self-test case(s) failed" % failed, file=sys.stderr)
        return 1
    print("check-muted-text-pair self-test OK (%d cases)" % len(cases))
    return 0

def scan(root):
    web = os.path.join(root, "web")
    scanned, hits = 0, []
    # Hand-written JS is scanned too; minified vendor bundles (*.min.js) are not.
    js_root = os.path.join(web, "static", "js")
    for dirpath, _dirs, files in os.walk(web):
        for name in sorted(files):
            is_templ = name.endswith(".templ")
            is_js = (name.endswith(".js") and not name.endswith(".min.js")
                     and (dirpath == js_root or dirpath.startswith(js_root + os.sep)))
            if not (is_templ or is_js):
                continue
            path = os.path.join(dirpath, name)
            scanned += 1
            with open(path, encoding="utf-8") as fh:
                text = fh.read()
            for line in find_pairs(text):
                hits.append("%s:%d" % (os.path.relpath(path, root), line))
    if scanned == 0:
        # A guard that examined nothing must not report OK.
        print("FAIL: no .templ or .js files found under %s; the guard scanned nothing" % web, file=sys.stderr)
        return 2
    if hits:
        for hit in sorted(hits):
            print("FAIL: %s: the failing muted-text pair (light gray-400 with dark gray-500) is on one class list" % hit, file=sys.stderr)
        print("HINT: use gray-600 in light and gray-400 in dark (#3474); the old pair paints under 4.5:1 on glass cards", file=sys.stderr)
        return 1
    print("check-muted-text-pair OK (%d .templ and non-minified .js files scanned)" % scanned)
    return 0

mode = os.environ["MODE"]
sys.exit(self_test() if mode == "self-test" else scan(os.environ["REPO_ROOT"]))
PY
