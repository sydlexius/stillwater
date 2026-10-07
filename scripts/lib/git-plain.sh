#!/usr/bin/env bash
#
# git-plain.sh -- run `git diff` with the developer's diff tooling switched off
# (#3446), for every check that PARSES the output.
#
# A gate verdict read from `git diff` text depends on that text being plain.
# Measured on git 2.x (each value below changes what a parser sees; names-only
# output is unaffected by the external tool and by color):
#   GIT_EXTERNAL_DIFF / diff.external / GIT_CONFIG_COUNT-injected diff.external
#                      -> the tool's output replaces the patch: no `+` lines.
#   color.ui / color.diff = always  -> escape codes before the `+`: `^+` misses.
#   a textconv driver (gitattributes diff=<name> + diff.<name>.textconv)
#                      -> the converter's text is diffed, not the file's.
#   GIT_DIFF_OPTS=-uN  -> OVERRIDES an explicit --unified=0: context lines
#                         appear and hunk-derived line numbers drift.
#   diff.noprefix / diff.mnemonicPrefix -> `diff --git` header loses `a/ b/`
#                         (or becomes `c/ w/`): path parsing breaks.
#   core.quotePath (default true) -> non-ASCII paths come back C-quoted, in the
#                         header AND in --name-only output.
#   diff.renames=false -> a pure rename reads as a whole new file of `+` lines.
#   a binary / -diff attribute or diff.<drv>.binary -> "Binary files differ",
#                         no `+` lines (--text).
#   diff.interHunkContext -> nearby --unified=0 hunks fuse, line numbers drift.
#   GIT_LITERAL_PATHSPECS / GIT_NOGLOB_PATHSPECS / GIT_GLOB_PATHSPECS /
#   GIT_ICASE_PATHSPECS -> pathspec'd diffs come back empty or widened.
#   a replace ref (git replace) -> the diff reads the replacement commit; the
#                         variable's mere presence disables replacement.
# This is a function, not a bare flag list: GIT_DIFF_OPTS is an environment
# variable and cannot be switched off by an argument.
#
# `git show <rev>:<path>` (a blob, no patch) was probed against all of the above
# except a replace ref, and is otherwise unaffected, so it needs no wrapper; a raw
# blob read in a gate that matters sets GIT_NO_REPLACE_OBJECTS=1 itself (the
# OpenAPI base read does). scripts/check-plain-git-diff.sh
# flags the common spellings of a patch-producing git call in scripts/ and
# .githooks/ that skip this helper; its header lists what it cannot see.
# Callers must also check git's own failure: capture the output in an
# assignment (`d=$(git_plain_diff ...)`) so a failed git is not read as "no diff".
#
# USAGE: . "$SCRIPT_DIR/lib/git-plain.sh"; git_plain_diff --name-only "$BASE" -- '*.go'

git_plain_diff() {
    GIT_DIFF_OPTS='' GIT_LITERAL_PATHSPECS=0 GIT_GLOB_PATHSPECS=0 \
    GIT_NOGLOB_PATHSPECS=0 GIT_ICASE_PATHSPECS=0 GIT_NO_REPLACE_OBJECTS=1 \
    command git -c core.quotePath=false diff \
        --no-ext-diff --no-textconv --no-color --text --inter-hunk-context=0 -M \
        --src-prefix=a/ --dst-prefix=b/ "$@"
}
