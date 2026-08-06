#!/bin/bash
# link-worktree-settings.sh -- point a sibling worktree's Claude Code
# permission grants at the main repo's copy (#2879).
#
# Claude Code reads project-local grants from <repo>/.claude/settings.local.json.
# That path is gitignored, so a `git worktree add` checkout never receives one,
# and an agent working in a fresh worktree silently falls back to the smaller
# user-global rule set. The visible symptom is a permission prompt for a command
# the repo demonstrably already grants -- and a BACKGROUNDED agent cannot answer
# a prompt, so it stalls indefinitely with no error anywhere.
#
# A SYMLINK, not a copy. One source of truth, so a grant added later in the main
# repo reaches every existing worktree with no further action. Independent copies
# drift, and an "always allow" click inside a worktree would accumulate in a file
# nothing audits -- which is how a blanket rule gets reintroduced unnoticed. The
# link is RELATIVE so it survives the whole tree being moved or renamed.
#
# This never widens the permission surface: it makes a worktree see the SAME
# curated file the main repo already uses, rather than falling back to a
# different set. It does not read, parse, or modify the file's contents.
#
# Usage:
#   link-worktree-settings.sh <worktree-dir> [main-repo-dir]
#
# main-repo-dir defaults to git's MAIN worktree, not to this script's own
# directory. Those differ in the case that actually happens: running
# `make worktree` from inside an existing worktree. The script would then look
# for settings in THAT worktree -- which has only a symlink, or nothing at all --
# and skip, leaving the new worktree unlinked while reporting success. The main
# worktree holds the one real file, so it is resolved explicitly.
#
# Exit status:
#   0 = linked, already correct, or nothing to link (no source file)
#   1 = refused: the destination exists and is NOT our symlink
#   2 = invalid input (missing or nonexistent worktree dir)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# git lists the MAIN worktree first, so its path is the first "worktree " line.
# Falls back to the script's own repo root when git cannot answer (not a
# repository, or git absent) -- correct whenever the two coincide, which is
# every case except being invoked from a linked worktree.
resolve_main_worktree() {
  local first
  # `awk 'NR==1'` WITHOUT `exit`, deliberately: an awk that exits early closes
  # the pipe, and with enough worktrees git then dies of SIGPIPE -- which under
  # `set -o pipefail` fails the whole substitution and silently drops us into
  # the fallback below. That fallback is the ORIGINAL #2879 bug, so the
  # micro-optimisation of exiting early could reintroduce the exact defect this
  # function exists to prevent. Reading the full list costs nothing at any
  # plausible worktree count.
  # `env -u GIT_DIR -u GIT_WORK_TREE`, and it is load-bearing. Git resolves
  # GIT_DIR BEFORE it honours `-C`, so an inherited one WINS: run from any git
  # hook (which exports both), `git -C "$SCRIPT_DIR" worktree list` answers for
  # whatever repository invoked the hook rather than for SCRIPT_DIR's. Caught by
  # the pre-push hook running this script's own test suite, where the fixture's
  # "main worktree" resolved to the real repository and the case failed. Left
  # unfixed, the script would link a worktree at the wrong repo's settings
  # whenever it ran under a hook.
  # The unset list is the FULL repository-selecting set, not just GIT_DIR.
  # GIT_COMMON_DIR selects another repository's worktree set outright, and
  # GIT_OBJECT_DIRECTORY / GIT_ALTERNATE_OBJECT_DIRECTORIES / GIT_INDEX_FILE
  # can make the command FAIL, which lands in the fallback below -- and that
  # fallback is the original #2879 bug. An incomplete unset here is therefore
  # not a partial fix; it is the same defect through a different variable.
  if first=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_INDEX_FILE \
      -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES -u GIT_PREFIX \
      git -C "$SCRIPT_DIR" worktree list --porcelain 2>/dev/null | awk '/^worktree /{if (!seen++) print substr($0, 10)}') \
      && [ -n "$first" ] && [ -d "$first" ]; then
    echo "$first"
    return 0
  fi
  (cd "$SCRIPT_DIR/.." && pwd -P)
}

# A link this script just created must actually RESOLVE to a readable file.
# Every failure mode here is otherwise SILENT: a dangling or self-referential
# link reads as "linked" to anyone glancing at `ls`, grants nothing, and
# surfaces days later as an agent that hung. `-e` follows the link, so it is
# false for both a dangling target and an ELOOP self-reference. This turns any
# future defect in the target computation into a loud failure at creation time
# instead of a quiet one at use time.
assert_resolves() {
  # Checks READABLE REGULAR FILE, not merely existence. `-e` alone was the
  # first version and it overstated itself in its own error message: a link to
  # a directory, or to a file the caller cannot read, passed `-e` and left the
  # worktree with no usable grants while this function reported success. `-f`
  # follows the link and requires a regular file; `-r` requires it readable.
  if [ ! -f "$1" ] || [ ! -r "$1" ]; then
    echo "FAIL: created a link at $1 that does not resolve to a readable file." >&2
    echo "      Target: $(readlink "$1" 2>/dev/null || echo '<unreadable>')" >&2
    echo "      Removing it rather than leaving a link that grants nothing." >&2
    rm -f "$1"
    exit 1
  fi
}

WORKTREE_DIR="${1:-}"
MAIN_DIR="${2:-$(resolve_main_worktree)}"

if [ -z "$WORKTREE_DIR" ]; then
  echo "usage: link-worktree-settings.sh <worktree-dir> [main-repo-dir]" >&2
  exit 2
fi
if [ ! -d "$WORKTREE_DIR" ]; then
  echo "FAIL: worktree directory does not exist: $WORKTREE_DIR" >&2
  exit 2
fi

# Resolve both to absolute PHYSICAL paths before comparing or computing the
# relative link: a caller passing `../stillwater-foo` (which `make worktree`
# does) would otherwise produce a link relative to the wrong directory.
#
# `pwd -P`, NOT bare `pwd`. Bare pwd is LOGICAL -- it preserves symlink
# components -- so two names for the same directory compare UNEQUAL and the
# self-link guard below waves them through. That is not hypothetical: a
# `~/dev -> ~/Developer` convenience link, a symlinked parent, an APFS
# firmlink, or macOS's own `/tmp` -> `/private/tmp` all produce it. The
# consequence is the worst failure this script can have -- `ln -sfn` repoints
# the destination AT ITSELF, every read of it then fails with ELOOP, the
# worktree silently has NO project grants, and the script reports "OK".
WORKTREE_DIR="$(cd "$WORKTREE_DIR" && pwd -P)"
if [ ! -d "$MAIN_DIR" ]; then
  echo "FAIL: main repo directory does not exist: $MAIN_DIR" >&2
  exit 2
fi
MAIN_DIR="$(cd "$MAIN_DIR" && pwd -P)"

SRC="$MAIN_DIR/.claude/settings.local.json"
DEST_DIR="$WORKTREE_DIR/.claude"
# Resolve DEST_DIR PHYSICALLY when it already exists. WORKTREE_DIR is physical,
# but appending `.claude` re-introduces a logical component if that directory is
# itself a symlink elsewhere -- and `ln -s` resolves a relative target against
# the REAL directory, not the logical path relative_path computed from. The
# target would then point somewhere else entirely. assert_resolves catches it
# (loudly, removing the link), so this is a fails-where-it-could-succeed bug
# rather than a silent one -- but succeeding is better.
if [ -d "$DEST_DIR" ]; then
  DEST_DIR="$(cd "$DEST_DIR" && pwd -P)"
fi
DEST="$DEST_DIR/settings.local.json"

# Linking a directory to itself would replace the real file with a link to
# itself. Only reachable by calling this on the main repo, which `make worktree`
# never does -- but the failure mode is destroying the grant file, so it is
# checked rather than assumed.
if [ "$WORKTREE_DIR" = "$MAIN_DIR" ]; then
  echo "SKIP: worktree and main repo are the same directory ($MAIN_DIR) -- nothing to link."
  exit 0
fi

# No source file is a legitimate state, not an error: a fresh clone that has
# never granted anything has no settings.local.json, and worktree creation must
# not fail because of that.
if [ ! -e "$SRC" ]; then
  echo "SKIP: no $SRC to link -- worktree will use user-global grants only."
  exit 0
fi

mkdir -p "$DEST_DIR"

# Compute the link target RELATIVE to the destination's directory, for ANY
# layout -- not just the sibling one `make worktree` produces.
#
# An earlier version special-cased siblings and fell back to an ABSOLUTE link
# otherwise. That fallback was a silent-failure mode wearing a "documented
# limitation" badge: move or rename the tree and the absolute link dangles,
# which presents identically to the bug this script exists to fix (a worktree
# with no project grants). The manual invocation in docs/worktrees.md can reach
# a non-sibling layout, so "unreachable through make worktree" was not a
# defence. A general computation is barely longer than the special case and
# removes the failure mode rather than documenting it.
#
# Hand-rolled rather than `realpath --relative-to`, which is GNU-only and absent
# on stock macOS. Strips the common prefix one path COMPONENT at a time (never
# by string length, which would confuse /a/bc with /a/b), then emits one `..`
# per remaining component of the source directory.
relative_path() {
  # $1 = from directory, $2 = to path. Both absolute and physical.
  local from="$1" to="$2" up=""
  while [ "$from" != "/" ] && [ "${to#"$from"/}" = "$to" ]; do
    from="$(dirname "$from")"
    up="../$up"
  done
  if [ "$from" = "/" ]; then
    # No shared component below the root: a relative path would be all `..`
    # and no clearer than an absolute one.
    echo "$to"
    return 0
  fi
  echo "$up${to#"$from"/}"
}

LINK_TARGET="$(relative_path "$DEST_DIR" "$SRC")"

if [ -L "$DEST" ]; then
  CURRENT="$(readlink "$DEST")"
  if [ "$CURRENT" = "$LINK_TARGET" ]; then
    echo "OK: $DEST already links to the main repo's settings."
    exit 0
  fi
  # Repointing OUR OWN symlink is safe -- no user data can be lost, since a
  # symlink holds none. This is the upgrade path for a worktree linked before
  # the relative-target change.
  ln -sfn "$LINK_TARGET" "$DEST"
  assert_resolves "$DEST"
  echo "OK: repointed $DEST -> $LINK_TARGET (was $CURRENT)."
  exit 0
fi

# A REGULAR FILE here is somebody's real, possibly diverged grants. Refuse
# rather than overwrite: the whole point of this issue is that hand-made copies
# accumulate unaudited grants, and silently deleting one would destroy the very
# evidence a reviewer would need. Fail LOUDLY and let a human decide.
if [ -e "$DEST" ]; then
  echo "FAIL: $DEST already exists and is not a link to the main repo." >&2
  echo "      It may hold grants that diverged from $SRC." >&2
  echo "      Inspect and remove it, then re-run, if you want the shared file." >&2
  exit 1
fi

ln -s "$LINK_TARGET" "$DEST"
assert_resolves "$DEST"
echo "OK: linked $DEST -> $LINK_TARGET"
