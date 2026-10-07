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
# This is a function, not a bare flag list: GIT_DIFF_OPTS is an environment
# variable and cannot be switched off by an argument.
#
# `git show <rev>:<path>` (a blob, no patch) was probed against all of the above
# and is unaffected, so it needs no wrapper. scripts/check-plain-git-diff.sh
# fails when a parsed `git diff` bypasses this helper.
#
# USAGE: . "$SCRIPT_DIR/lib/git-plain.sh"; git_plain_diff --name-only "$BASE" -- '*.go'

git_plain_diff() {
    GIT_DIFF_OPTS='' git -c core.quotePath=false diff \
        --no-ext-diff --no-textconv --no-color -M \
        --src-prefix=a/ --dst-prefix=b/ "$@"
}
