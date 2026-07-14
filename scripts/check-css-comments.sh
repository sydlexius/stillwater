#!/usr/bin/env bash
# check-css-comments.sh -- assert that no hand-written CSS comment terminates
# itself, silently turning the prose that follows it into CSS.
#
# THE BUG THIS EXISTS FOR (#2525). design-tokens.css carried this line inside a
# block comment:
#
#     * namespace mints the full bg-*/text-*/border-* utility set per variable
#
# `*/` CLOSES a CSS block comment. The comment therefore ended in the middle of
# that glob, and every character after it stopped being prose and became raw CSS.
# That garbage swallowed the `@theme` block on the next line, so the design-system
# utilities it publishes were NEVER GENERATED. There was no visual damage, because
# nothing consumed those utilities yet -- the harm was that a design-system
# guardrail was silently off. The next person to reach for one of those utility
# classes would have got no style AND no error, which is the worst failure mode a
# guardrail can have. The only signal was a Tailwind build warning, and build
# warnings are background noise.
#
# THIS FILE DELIBERATELY NAMES NO UTILITY CLASS. Tailwind v4's automatic content
# detection scans the whole project -- shell scripts included -- and harvests
# anything that looks like a class name, EVEN INSIDE A COMMENT. An earlier draft of
# this header spelled out the token-backed class names it was describing, and
# Tailwind duly minted them into the shipped stylesheet: this guard was editing
# web/static/css/styles.css through nothing but its own documentation. Describe the
# utilities; do not write them. (Tailwind does not scan .css files, which is why
# the same names sitting in design-tokens.css prose are inert.)
#
# WHAT IT CHECKS, AND WHY IT IS NOT A GREP. You cannot grep for "a comment body
# containing `*/`" -- a comment body CANNOT contain `*/`, because that sequence
# is precisely what ends it. There is nothing to match. So this scans for the
# CONSEQUENCE instead:
#
#   A `*/` that appears while NOT inside a block comment.
#
# When a comment self-terminates early, the author's INTENDED closing `*/` (and,
# in the #2525 line, the very next `*/` in the same sentence) is left stranded in
# code context. A stray `*/` outside a comment is not valid CSS, so the signal is
# strong rather than heuristic: there are no thresholds to tune and nothing for a
# future author to appease.
#
# It is not, however, a total decision procedure, and the header should not claim
# to be one. Two known gaps, both accepted deliberately:
#   - If the stranded prose happens to contain an apostrophe, string state opens
#     and can swallow the intended closing `*/`, so the guard stays quiet. That
#     shape does not go silent in the build the way #2525 did: Tailwind hard-errors
#     on it (CssSyntaxError, no output), which is a loud failure, not a silent one.
#     The guard covers the multi-line shape that IS silent.
#   - An unquoted `url(...)` containing a literal `*/`, and a backslash-continued
#     string spanning a newline, both read as strays. Legal CSS, but neither occurs
#     here, and both are the price of the end-of-line string reset below -- a trade
#     made deliberately in favor of not letting one unbalanced quote poison a file.
#
# The same scanner reports an unterminated comment at EOF, which is the other
# way this class of typo lands (`/*` opened, never closed) and is likewise
# always a bug.
#
# SCOPE. Hand-written CSS only, discovered from git rather than hard-coded so a
# newly added stylesheet is covered automatically. Two exclusions:
#   - *.min.css      -- vendored third-party (cropper, driver). We do not author
#                       them, and a false positive would block a vendor bump.
#   - styles.css     -- GENERATED Tailwind output. Its inputs are checked here,
#                       which is the authoritative surface; checking the build
#                       product too would just report the same defect twice.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# --others --exclude-standard unions in a brand-new, not-yet-added stylesheet, so
# the guard fires on the commit that INTRODUCES the defect rather than the one
# after it.
#
# Read with a while-read loop, NOT `mapfile`: this repo targets Bash 3.2 (stock
# macOS /bin/bash), where `mapfile` does not exist -- see the same convention
# stated in scripts/coverage-floor.sh. pre-push-gate.sh invokes this script as
# `bash <path>`, so on a machine whose PATH bash IS /bin/bash, a `mapfile` here
# would abort with "command not found" and block EVERY push. A pre-push guard is
# the last place that failure mode is acceptable.
css_files=()
while IFS= read -r line; do
  css_files+=("$line")
done < <(
  {
    git ls-files -- '*.css'
    git ls-files --others --exclude-standard -- '*.css'
  } | sort -u | grep -v '\.min\.css$' | grep -v '^web/static/css/styles\.css$'
)

if [ ${#css_files[@]} -eq 0 ]; then
  echo "check-css-comments: no hand-written CSS found (nothing to check)"
  exit 0
fi

failed=0

for f in "${css_files[@]}"; do
  [ -f "$f" ] || continue

  # A three-state character scanner: code / comment / string.
  #
  # In CODE we watch for `/*` (open a comment), a quote (open a string -- so a
  # `*/` inside `content: "*/"` is not misread as a stray), and `*/` itself,
  # which is the defect.
  #
  # In COMMENT we watch only for `*/`. CSS comments do not nest, so a `/*` in
  # here is just prose.
  #
  # A CSS string cannot span a raw newline, so the string state is reset at
  # end-of-line: one unbalanced quote cannot poison the rest of the file.
  # sq is passed in rather than written as an escape: `\x27` is a non-POSIX awk
  # extension, and CI runs on mawk while dev machines run BSD awk or gawk. If an
  # implementation did not honor it, single-quoted strings would stop being
  # tracked and a `*/` inside one would be reported as a stray -- a false
  # positive, the one failure mode this guard cannot afford.
  awk -v file="$f" -v sq="'" '
    BEGIN { state = "code"; open_line = 0; delim = "" }
    {
      n = length($0)
      i = 1
      while (i <= n) {
        c = substr($0, i, 1)
        d = (i < n) ? substr($0, i + 1, 1) : ""

        if (state == "code") {
          if (c == "/" && d == "*") { state = "comment"; open_line = FNR; i += 2; continue }
          if (c == "*" && d == "/") {
            printf "%s:%d: stray `*/` outside a comment -- a comment above terminated itself\n", file, FNR
            print "  " $0
            bad = 1
            i += 2
            continue
          }
          if (c == "\"" || c == sq) { state = "string"; delim = c; i += 1; continue }
          i += 1
          continue
        }

        if (state == "comment") {
          if (c == "*" && d == "/") { state = "code"; i += 2; continue }
          i += 1
          continue
        }

        # state == "string"
        if (c == "\\") { i += 2; continue }
        if (c == delim) { state = "code"; i += 1; continue }
        i += 1
      }
      if (state == "string") state = "code"
    }
    END {
      if (state == "comment") {
        printf "%s:%d: block comment opened here is never closed\n", file, open_line
        bad = 1
      }
      exit bad ? 1 : 0
    }
  ' "$f" || failed=1
done

if [ "$failed" -ne 0 ]; then
  cat >&2 <<'EOF'

ERROR: self-terminating CSS comment (see #2525).

A `*/` inside comment PROSE closes the comment. Everything after it stops being
a comment and is parsed as CSS -- which silently swallows whatever follows
(in #2525, an entire `@theme` block, so its utilities were never generated).

Writing a utility-prefix glob in a comment is the usual way in: a `*` immediately
followed by a `/` ends the comment right there. Reword the prose so the two
characters never touch -- spell the prefixes out as words instead of globbing
them.

There is no escape for `*/` inside a CSS comment. Rewording is the only fix.
EOF
  exit 1
fi

echo "CSS comments: OK (${#css_files[@]} file(s) scanned)"
