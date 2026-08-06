#!/usr/bin/env bash
#
# test-link-worktree-settings.sh -- tests for scripts/link-worktree-settings.sh (#2879).
#
# Hermetic. Every case builds throwaway "main repo" and "worktree" directories
# under a temp dir; nothing touches the developer's real .claude/settings.local.json,
# which is never read, written, or even opened by these tests. The script under
# test only ever creates a symlink, so the fixtures need no git repository at all.
#
# Run: bash scripts/test-link-worktree-settings.sh
#
# Every case asserts its own preconditions before asserting its result. The
# failure mode this guards against -- a worktree silently running on the wrong
# permission set -- is invisible at runtime, so a test that passed because its
# setup quietly did nothing would be worse than no test.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LINK="$REPO_ROOT/scripts/link-worktree-settings.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# Isolate every git invocation below -- the fixtures AND the script under test --
# from an inherited git environment. Git resolves GIT_DIR before it honours
# `-C`, so a hook-supplied GIT_DIR WINS and the fixture silently operates on the
# REAL repository instead of its own. Not hypothetical: the pre-push hook runs
# this suite, and without these unsets the default-source case read the real
# repo's settings.local.json rather than its fixture marker, failing 19/1 where
# a direct run passed 20/0. A suite that only holds when invoked by hand is not
# hermetic, and the gate is exactly where it needs to be trusted.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY
unset GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_PREFIX
# Fixtures must not inherit the developer's global config either (signing hooks,
# excludes, templates); the excludes in particular decide whether the fixture's
# settings file is committed, which is what makes the default-source case mean
# anything. Same discipline as test-check-commit-signing.sh.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

PASSED=0
FAILED=0

ok() {
    echo "  PASS  $1"
    PASSED=$((PASSED + 1))
}

bad() {
    echo "  FAIL  $1" >&2
    [ $# -gt 1 ] && printf '        %s\n' "${@:2}" >&2
    FAILED=$((FAILED + 1))
}

# A precondition that does not hold invalidates the case rather than failing it.
# Reported loudly and counted as a failure -- never skipped quietly.
require() {
    if ! eval "$1"; then
        bad "PRECONDITION FAILED: $1" "${@:2}"
        return 1
    fi
    return 0
}

# new_pair <case-name> [--no-settings]
# Builds a sibling main/worktree pair under a per-case parent, mirroring the real
# layout (../stillwater-<slug> next to stillwater/). Echoes the parent dir.
# The settings file carries a marker string so a test can prove WHICH file it
# ended up reading through the link.
new_pair() {
    local name="$1" parent
    parent="$WORK/$name"
    # The worktree side deliberately gets NO .claude directory: the script has
    # to create it, and a fixture that pre-created one hid that (mutating the
    # mkdir away survived the whole suite). The cases that need a pre-existing
    # destination make it themselves.
    mkdir -p "$parent/stillwater/.claude" "$parent/stillwater-wt"
    if [ "${2:-}" != "--no-settings" ]; then
        printf '{"marker":"MAIN-REPO-GRANTS"}\n' > "$parent/stillwater/.claude/settings.local.json"
    fi
    echo "$parent"
}

echo "link-worktree-settings.sh"
echo

# --------------------------------------------------------------------------
# The core case: a fresh worktree gets a working link.
# --------------------------------------------------------------------------
p=$(new_pair basic)
if require "[ ! -e '$p/stillwater-wt/.claude/settings.local.json' ]" \
        "the worktree must START without settings, or this proves nothing"; then
    if "$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 \
        && [ -L "$p/stillwater-wt/.claude/settings.local.json" ]; then
        ok "creates a symlink in a worktree that had none"
    else
        bad "creates a symlink in a worktree that had none"
    fi

    # The link must RESOLVE and deliver the main repo's content. A dangling
    # symlink is still a symlink: asserting -L alone would pass on one that
    # points nowhere, and an agent would then run on global grants exactly as
    # if the fix had never landed.
    got=$(cat "$p/stillwater-wt/.claude/settings.local.json" 2>/dev/null || echo UNREADABLE)
    if [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
        ok "the link resolves and reads the main repo's file"
    else
        bad "the link resolves and reads the main repo's file" "got: $got"
    fi
fi

# --------------------------------------------------------------------------
# The link must be RELATIVE, so moving or renaming the whole tree does not
# break every worktree at once. This is the property that makes a symlink
# better than an absolute path, so it is asserted directly rather than assumed.
# --------------------------------------------------------------------------
p=$(new_pair relative)
"$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 || true
target=$(readlink "$p/stillwater-wt/.claude/settings.local.json" 2>/dev/null || echo NONE)
case "$target" in
    /*) bad "link target is relative" "got an ABSOLUTE path: $target" ;;
    ../../stillwater/.claude/settings.local.json) ok "link target is relative" ;;
    *)  bad "link target is relative" "unexpected target: $target" ;;
esac

# Prove the relative link actually survives a move, rather than trusting that a
# string starting with ".." implies it.
mv "$p" "$p-moved"
got=$(cat "$p-moved/stillwater-wt/.claude/settings.local.json" 2>/dev/null || echo UNREADABLE)
if [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
    ok "link still resolves after the tree is moved"
else
    bad "link still resolves after the tree is moved" "got: $got"
fi

# --------------------------------------------------------------------------
# A NON-SIBLING layout must also get a RELATIVE, move-surviving link.
#
# The first implementation special-cased siblings and fell back to an ABSOLUTE
# link for anything else -- a silent-failure mode with a "documented limitation"
# badge on it, since moving the tree leaves a dangling link that presents
# exactly like the bug this script fixes. `make worktree` only makes siblings,
# but the manual invocation documented in docs/worktrees.md can reach this.
# --------------------------------------------------------------------------
np="$WORK/nonsibling"
mkdir -p "$np/main/.claude" "$np/sub/deep/wt"
printf '{"marker":"MAIN-REPO-GRANTS"}\n' > "$np/main/.claude/settings.local.json"
"$LINK" "$np/sub/deep/wt" "$np/main" >/dev/null 2>&1 || true
target=$(readlink "$np/sub/deep/wt/.claude/settings.local.json" 2>/dev/null || echo NONE)
case "$target" in
    /*)   bad "a non-sibling layout still gets a RELATIVE link" "got an ABSOLUTE path: $target" ;;
    NONE) bad "a non-sibling layout still gets a RELATIVE link" "no link was created" ;;
    *)    ok "a non-sibling layout still gets a RELATIVE link" ;;
esac
mv "$np" "$np-moved"
got=$(cat "$np-moved/sub/deep/wt/.claude/settings.local.json" 2>/dev/null || echo DANGLING)
if [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
    ok "the non-sibling link survives a move"
else
    bad "the non-sibling link survives a move" "got: $got"
fi

# --------------------------------------------------------------------------
# Idempotence: make worktree may be re-run, and a second call must not fail or
# churn the link.
# --------------------------------------------------------------------------
p=$(new_pair idempotent)
"$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 || true
first=$(readlink "$p/stillwater-wt/.claude/settings.local.json" 2>/dev/null || echo NONE)
if out=$("$LINK" "$p/stillwater-wt" "$p/stillwater" 2>&1) \
    && [ "$(readlink "$p/stillwater-wt/.claude/settings.local.json")" = "$first" ] \
    && printf '%s' "$out" | grep -q 'already links'; then
    ok "running twice is a no-op that reports the existing link"
else
    bad "running twice is a no-op that reports the existing link" "$out"
fi

# --------------------------------------------------------------------------
# REFUSES to clobber a real file. This is the case that protects the thing the
# issue is actually about: a hand-made copy may hold grants that diverged from
# the main repo's, and deleting one silently would destroy the only evidence
# that divergence ever happened.
# --------------------------------------------------------------------------
p=$(new_pair refuses)
mkdir -p "$p/stillwater-wt/.claude"
printf '{"marker":"HAND-MADE-DIVERGED-COPY"}\n' > "$p/stillwater-wt/.claude/settings.local.json"
if require "[ -f '$p/stillwater-wt/.claude/settings.local.json' ] && [ ! -L '$p/stillwater-wt/.claude/settings.local.json' ]" \
        "the destination must be a REGULAR FILE for this case to mean anything"; then
    rc=0
    out=$("$LINK" "$p/stillwater-wt" "$p/stillwater" 2>&1) || rc=$?
    if [ "$rc" -eq 1 ]; then
        ok "refuses (exit 1) when the destination is a regular file"
    else
        bad "refuses (exit 1) when the destination is a regular file" "exit was $rc"
    fi
    # ASSERT THE EXPLANATION, not just the exit code. Deleting the guard
    # entirely still yields exit 1 and an intact file, because plain `ln -s`
    # refuses to overwrite on its own -- so the two assertions above cannot
    # tell a deliberate refusal from an accidental one, and both pass against
    # a script that lost the guard. The difference an operator sees is the
    # message: "may hold grants that diverged" tells them WHY and what to do,
    # where `ln: File exists` tells them nothing. Pinning the wording is what
    # makes the guard's own existence testable.
    if printf '%s' "$out" | grep -q 'diverged'; then
        ok "the refusal explains WHY, rather than leaking a bare ln error"
    else
        bad "the refusal explains WHY, rather than leaking a bare ln error" "$out"
    fi
    # The refusal is only worth anything if the file SURVIVED it.
    got=$(cat "$p/stillwater-wt/.claude/settings.local.json")
    if [ "$got" = '{"marker":"HAND-MADE-DIVERGED-COPY"}' ]; then
        ok "the pre-existing file is left untouched"
    else
        bad "the pre-existing file is left untouched" "content changed to: $got"
    fi
fi

# --------------------------------------------------------------------------
# A stale symlink from an older layout IS repointed -- safe, since a symlink
# holds no user data. This is the upgrade path, and it must not be confused
# with the regular-file case above.
# --------------------------------------------------------------------------
p=$(new_pair repoint)
mkdir -p "$p/stillwater-wt/.claude"
ln -s /nonexistent/old/path "$p/stillwater-wt/.claude/settings.local.json"
if require "[ -L '$p/stillwater-wt/.claude/settings.local.json' ]" "destination must start as a symlink"; then
    if "$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 \
        && [ "$(cat "$p/stillwater-wt/.claude/settings.local.json")" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
        ok "repoints a stale symlink to the current target"
    else
        bad "repoints a stale symlink to the current target"
    fi
fi

# --------------------------------------------------------------------------
# No source file is a legitimate state (a fresh clone that has granted nothing).
# Worktree creation must not fail because of it.
# --------------------------------------------------------------------------
p=$(new_pair nosource --no-settings)
if require "[ ! -e '$p/stillwater/.claude/settings.local.json' ]" "main repo must have NO settings file"; then
    rc=0
    out=$("$LINK" "$p/stillwater-wt" "$p/stillwater" 2>&1) || rc=$?
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'SKIP'; then
        ok "exits 0 with SKIP when the main repo has no settings file"
    else
        bad "exits 0 with SKIP when the main repo has no settings file" "exit $rc: $out"
    fi
    if [ ! -e "$p/stillwater-wt/.claude/settings.local.json" ]; then
        ok "creates no dangling link when there is no source"
    else
        bad "creates no dangling link when there is no source"
    fi
fi

# --------------------------------------------------------------------------
# Self-link guard: pointing the script at the main repo itself would replace the
# real grants file with a link to itself. Unreachable from `make worktree`, but
# the blast radius is destroying the file, so it is checked.
# --------------------------------------------------------------------------
p=$(new_pair selflink)
rc=0
"$LINK" "$p/stillwater" "$p/stillwater" >/dev/null 2>&1 || rc=$?
got=$(cat "$p/stillwater/.claude/settings.local.json" 2>/dev/null || echo DESTROYED)
if [ "$rc" -eq 0 ] && [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ] \
    && [ ! -L "$p/stillwater/.claude/settings.local.json" ]; then
    ok "refuses to link the main repo to itself, leaving the real file intact"
else
    bad "refuses to link the main repo to itself, leaving the real file intact" "exit $rc, content: $got"
fi

# --------------------------------------------------------------------------
# PATH ALIASING must not defeat the self-link guard.
#
# The guard compares resolved paths, and `cd X && pwd` is LOGICAL -- it keeps
# symlink components -- so two names for the SAME directory compared unequal
# and the guard waved them through. `ln -sfn` then repointed the destination AT
# ITSELF: every read fails with ELOOP, the worktree silently has NO grants, and
# the script printed "OK: repointed". Reproduced before the fix. Aliases are
# ordinary: a ~/dev -> ~/Developer convenience link, a symlinked parent, an APFS
# firmlink, or macOS's own /tmp -> /private/tmp.
#
# Fixed with `pwd -P`. This case pins it, and the resolve assertion is the
# backstop that makes any residual variant loud instead of silent.
# --------------------------------------------------------------------------
p=$(new_pair alias)
mkdir -p "$p/real"
mv "$p/stillwater" "$p/real/stillwater"
mv "$p/stillwater-wt" "$p/real/stillwater-wt"
ln -s real "$p/aka"
"$LINK" "$p/real/stillwater-wt" "$p/real/stillwater" >/dev/null 2>&1 || true
if require "[ \"\$(cat '$p/real/stillwater-wt/.claude/settings.local.json' 2>/dev/null)\" = '{\"marker\":\"MAIN-REPO-GRANTS\"}' ]" \
        "the worktree must be correctly linked BEFORE the aliased call, or the case proves nothing"; then
    # Same directory, reached by its aliased name. A self-link must be refused.
    "$LINK" "$p/aka/stillwater-wt" "$p/real/stillwater-wt" >/dev/null 2>&1 || true
    got=$(cat "$p/real/stillwater-wt/.claude/settings.local.json" 2>/dev/null || echo ELOOP-OR-DANGLING)
    if [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
        ok "an aliased path does not defeat the self-link guard"
    else
        bad "an aliased path does not defeat the self-link guard" \
            "the link no longer resolves (got: $got) -- the worktree has NO grants"
    fi
fi

# --------------------------------------------------------------------------
# The destination .claude directory need not already exist. Worth pinning
# separately: mutating the `mkdir -p` away survived the entire suite while
# breaking real use, because every fixture used to pre-create the directory.
# --------------------------------------------------------------------------
p=$(new_pair nodir)
if require "[ ! -d '$p/stillwater-wt/.claude' ]" "the worktree must start with NO .claude directory"; then
    if "$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 \
        && [ "$(cat "$p/stillwater-wt/.claude/settings.local.json" 2>/dev/null)" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
        ok "creates the .claude directory when it does not exist"
    else
        bad "creates the .claude directory when it does not exist"
    fi
fi

# --------------------------------------------------------------------------
# assert_resolves must require a READABLE REGULAR FILE, not mere existence.
# `-e` alone (the first version) passed on a link to a DIRECTORY, leaving the
# worktree with no usable grants while the script reported success -- the error
# message said "readable file" and the check did not deliver it.
# --------------------------------------------------------------------------
p=$(new_pair dirtarget --no-settings)
mkdir -p "$p/stillwater/.claude/settings.local.json"   # a DIRECTORY at the source path
rc=0
"$LINK" "$p/stillwater-wt" "$p/stillwater" >/dev/null 2>&1 || rc=$?
if [ "$rc" -ne 0 ] && [ ! -e "$p/stillwater-wt/.claude/settings.local.json" ]; then
    ok "refuses a link whose target is not a readable regular file"
else
    bad "refuses a link whose target is not a readable regular file" \
        "exit $rc; a link to a DIRECTORY grants nothing but passes a bare -e check"
fi

# --------------------------------------------------------------------------
# Bad input.
# --------------------------------------------------------------------------
rc=0; "$LINK" >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 2 ]; then
    ok "exits 2 with no arguments"
else
    bad "exits 2 with no arguments" "exit was $rc"
fi

rc=0; "$LINK" "$WORK/does-not-exist" >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 2 ]; then
    ok "exits 2 on a nonexistent worktree dir"
else
    bad "exits 2 on a nonexistent worktree dir" "exit was $rc"
fi

# --------------------------------------------------------------------------
# DEFAULT SOURCE RESOLUTION, run from inside a LINKED WORKTREE.
#
# Every case above PASSES the main repo in explicitly, so none of them can see a
# wrong default -- and the default was in fact wrong: the script originally used
# its own directory, so `make worktree` invoked from an existing worktree looked
# for settings THERE (a symlink, or nothing), skipped, and reported success while
# linking nothing. Found by running the real make target, not by these tests,
# which is why this case exists.
#
# Needs real git worktrees, so it is skipped where git is unavailable rather
# than failing the suite for an environmental reason.
# --------------------------------------------------------------------------
if command -v git >/dev/null 2>&1; then
    gp="$WORK/gitdefault"
    mkdir -p "$gp/stillwater"
    (
        cd "$gp/stillwater"
        git init -q -b main .
        git config user.email t@t; git config user.name t; git config commit.gpgsign false
        mkdir -p .claude scripts
        # The fixture MUST carry its own .gitignore. Without one, whether the
        # settings file gets committed -- and so whether `git worktree add`
        # delivers a copy into the linked worktree -- depends on the developer's
        # GLOBAL excludes. Where it is not ignored, the file lands in the linked
        # worktree, the broken default finds settings there, and this case passes
        # against the very bug it exists to catch. Verified: with the global
        # ignore neutralized the file is staged; with this line it never is.
        printf '.claude/\n' > .gitignore
        printf '{"marker":"MAIN-REPO-GRANTS"}\n' > .claude/settings.local.json
        cp "$LINK" scripts/link-worktree-settings.sh
        echo x > f.txt
        git add -A && git commit -qm base
        # A LINKED worktree, standing in for "an agent runs make worktree from
        # the worktree it is already working in".
        git worktree add -q ../stillwater-linked -b linked
    ) >/dev/null 2>&1

    if require "[ -d '$gp/stillwater-linked' ]" "the linked worktree fixture must exist"; then
        mkdir -p "$gp/stillwater-target/.claude"
        # NOTE: no second argument -- the default is what is under test.
        ( cd "$gp/stillwater-linked" && ./scripts/link-worktree-settings.sh "$gp/stillwater-target" ) >/dev/null 2>&1 || true
        got=$(cat "$gp/stillwater-target/.claude/settings.local.json" 2>/dev/null || echo NOT-LINKED)
        if [ "$got" = '{"marker":"MAIN-REPO-GRANTS"}' ]; then
            ok "defaults to the MAIN worktree's settings when run from a linked worktree"
        else
            bad "defaults to the MAIN worktree's settings when run from a linked worktree" \
                "got: $got (a SKIP here means it resolved the source to the linked worktree)"
        fi
    fi
else
    echo "  SKIP  default-source case (git not available)"
fi

# --------------------------------------------------------------------------
# The make target must actually CALL this script. A perfect script that nothing
# invokes fixes nothing, and that wiring is the whole deliverable of #2879.
# --------------------------------------------------------------------------
# Asserts a RECIPE LINE, not any mention of the filename. A bare
# `grep -q 'link-worktree-settings.sh'` passes on a COMMENT containing the name,
# so this case would go quiet the moment somebody documented the step in prose --
# and the M4 comment added to this very target nearly did exactly that. A make
# recipe line is tab-prefixed, which is what distinguishes it from a comment.
# awk rather than `grep -P`: macOS ships BSD grep, which has no -P.
if awk '/^\t.*link-worktree-settings\.sh/ { found = 1 } END { exit !found }' "$REPO_ROOT/Makefile"; then
    ok "the worktree make target invokes this script"
else
    bad "the worktree make target invokes this script" \
        "no tab-prefixed recipe line in the Makefile runs scripts/link-worktree-settings.sh"
fi

echo
echo "passed: $PASSED   failed: $FAILED"
[ "$FAILED" -eq 0 ]
