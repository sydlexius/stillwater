#!/usr/bin/env bash
#
# check-commit-signing.sh -- refuse to create an unsigned commit in a repository
# that requires signed commits.
#
# Two modes, for the two stages at which the question can be asked:
#
#   (default, --probe)  Called by .githooks/pre-commit. Runs BEFORE the commit
#       object exists, so it cannot inspect the commit itself; instead it proves
#       that the signing path configured for this repository actually works right
#       now, by performing a real signature in a throwaway repository (the
#       "probe" below).
#
#   --head              Called by .githooks/post-commit. Runs AFTER the commit
#       object exists and reads the signature straight off it. This is the only
#       stage that can catch `git commit --no-gpg-sign`: that is a command-line
#       flag, not config, and git does not expose it to hooks in any form, so the
#       pre-commit mode sees commit.gpgsign=true, proves the signer works, and
#       passes while the commit being created is unsigned. That flag is the
#       actual mechanism behind #2624, so the pair is not optional -- the probe
#       mode alone misses the very case this check exists for.
#
# The modes are complements, not alternatives: --probe explains WHY signing will
# fail before it costs anything, --head proves whether it actually did.
#
# Why this exists (#2625): main is protected by a `required_signatures` ruleset,
# but nothing local enforced it. An unsigned commit committed, pushed, and passed
# every CI check, then blocked at the merge gate with no stated reason -- by which
# point the only fix was rewriting shared history on a reviewed PR (#2624). At
# commit time the same fix costs one `git commit` retry.
#
# This is the ADVISORY layer. It lives on the developer's machine, so it can be
# skipped with --no-verify or simply never installed (which is what `make doctor`
# checks for). The unforgeable layer is the required "Signed Commits" CI check in
# .github/workflows/signed-commits.yml, which runs where the committer has no say.
#
# Exit codes: 0 = signing verified, or not required. 1 = would produce an unsigned
# commit (or the signer is unreachable). There is no "could not tell" exit -- an
# indeterminate result is reported as a failure, never as a silent pass.

set -euo pipefail

MODE="probe"
case "${1:-}" in
    "" | --probe) ;;
    --head) MODE="head" ;;
    *)
        echo "check-commit-signing.sh: unknown argument '$1' (expected --probe or --head)" >&2
        exit 1
        ;;
esac

# Colors (disabled if not a terminal) -- matches .githooks/pre-commit.
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BOLD='\033[1m'
    RESET='\033[0m'
else
    RED='' GREEN='' YELLOW='' BOLD='' RESET=''
fi

# Every failure ends with the same remedy block, so the message is actionable
# wherever the check fires from.
remedies() {
    echo ""
    echo "Remedies, in the order worth trying:"
    echo ""
    if [ "$MODE" = "head" ]; then
        echo "  0. Re-sign the commit that was just created. It is local and"
        echo "     unpushed, so this costs one command and nothing else:"
        echo "       git commit --amend -S --no-edit"
        echo "     Then confirm it took:"
        echo "       git cat-file commit HEAD | sed -n '1,/^\$/p' | grep '^gpgsig'"
        echo "     If the amend still lands unsigned, work through the steps below"
        echo "     first -- something in the signing setup is genuinely broken."
        echo ""
    fi
    echo "  1. Confirm this repository is configured to sign:"
    echo "       git config commit.gpgsign true"
    echo "       git config --get user.signingkey"
    echo "     A worktree created before the signing config was set is the most"
    echo "     common cause -- worktrees do NOT inherit repo-local config."
    echo ""
    echo "  2. Confirm the signer itself is reachable. This repository signs with:"
    echo "       gpg.format        = $(git config --get gpg.format || echo '(unset -- defaults to openpgp)')"
    echo "       gpg.ssh.program   = $(git config --get gpg.ssh.program || echo '(unset)')"
    echo "     For the 1Password signer (op-ssh-sign), the 1Password desktop app"
    echo "     must be RUNNING and UNLOCKED. It talks to the app over its own IPC."
    echo ""
    echo "  3. For a plain ssh-agent setup (not op-ssh-sign), the agent socket must"
    echo "     be reachable. Non-interactive and agent-driven shells frequently get"
    echo "     an empty SSH_AUTH_SOCK. Export it before committing:"
    echo "       export SSH_AUTH_SOCK=\"\$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock\""
    echo "     Verify with: ssh-add -L"
    echo ""
    echo "Do NOT work around this with --no-verify or --no-gpg-sign. An unsigned"
    echo "commit is not rejected until the merge gate, where the only fix is"
    echo "rewriting history on a reviewed PR (#2624)."
}

fail() {
    echo -e "${RED}${BOLD}FAIL${RESET} signed-commits: $1" >&2
    shift
    for line in "$@"; do
        echo "  $line" >&2
    done
    remedies >&2
    exit 1
}

REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || {
    echo -e "${RED}${BOLD}FAIL${RESET} signed-commits: not inside a git repository." >&2
    exit 1
}

# --------------------------------------------------------------------------
# 1. Is signing required here?
# --------------------------------------------------------------------------
# Deliberately NOT inferred from commit.gpgsign. The requirement is a property of
# the repository; commit.gpgsign is a property of one clone. Inferring the
# requirement from the setting would make the check vacuous in exactly the case
# it exists to catch -- signing accidentally off -- because the check would
# conclude "signing is not required here" and pass.
#
# SW_REQUIRE_SIGNED_COMMITS overrides the marker file (used by the test suite,
# and available as a documented escape hatch).
MARKER="$REPO_ROOT/.githooks/signed-commits-required"
case "${SW_REQUIRE_SIGNED_COMMITS:-}" in
    1 | true | yes) REQUIRED=1 ;;
    0 | false | no) REQUIRED=0 ;;
    "") if [ -f "$MARKER" ]; then REQUIRED=1; else REQUIRED=0; fi ;;
    *) fail "SW_REQUIRE_SIGNED_COMMITS is set to '${SW_REQUIRE_SIGNED_COMMITS}'," \
        "which is not one of: 1 true yes 0 false no." ;;
esac

if [ "$REQUIRED" -eq 0 ]; then
    echo -e "${YELLOW}SKIP${RESET} signed-commits (not required by this repository)"
    exit 0
fi

# --------------------------------------------------------------------------
# 1h. --head: the commit object exists -- does it actually carry a signature?
# --------------------------------------------------------------------------
# The authoritative answer, and the only one that survives `--no-gpg-sign`.
# Nothing here reads commit.gpgsign: config describes intent, the object records
# what happened, and the whole point of this mode is that the two can disagree.
#
# Read via the RAW COMMIT OBJECT for the same reason the probe does -- see the
# long note on `%G?` further down.
if [ "$MODE" = "head" ]; then
    if ! git rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
        fail "asked to inspect HEAD, but this repository has no commits." \
            "This mode is meant to run from post-commit, where HEAD always exists."
    fi
    HEAD_SHA=$(git rev-parse --short HEAD)
    if git cat-file commit HEAD | sed -n '1,/^$/p' | grep -q '^gpgsig'; then
        echo -e "${GREEN}PASS${RESET} signed-commits (HEAD $HEAD_SHA carries a signature)"
        exit 0
    fi
    fail "the commit you just created ($HEAD_SHA) has NO signature." \
        "This repository requires signed commits, so it cannot merge as-is." \
        "" \
        "commit.gpgsign is '$(git config --get commit.gpgsign || echo unset)', so the" \
        "most likely cause is that signing was disabled for this one call --" \
        "'git commit --no-gpg-sign' or '-c commit.gpgsign=false'. Neither is" \
        "visible to a pre-commit hook, which is why this check runs here instead."
fi

# --------------------------------------------------------------------------
# 2. Is this clone configured to sign at all?
# --------------------------------------------------------------------------
# This is the #2624 case: signing silently off for one commit. Checked before the
# probe because the probe would happily succeed here -- it proves the signer
# WORKS, not that git will be asked to USE it.
GPGSIGN=$(git config --get commit.gpgsign 2>/dev/null || echo "")
case "$GPGSIGN" in
    true | yes | on | 1) ;;
    *) fail "this repository requires signed commits, but commit.gpgsign is '${GPGSIGN:-unset}'." \
        "git would create this commit WITHOUT a signature." ;;
esac

SIGNING_KEY=$(git config --get user.signingkey 2>/dev/null || echo "")
if [ -z "$SIGNING_KEY" ]; then
    fail "commit.gpgsign is true but user.signingkey is unset." \
        "git has no key to sign with."
fi

# --------------------------------------------------------------------------
# 3. Probe: does the configured signer actually produce a signature right now?
# --------------------------------------------------------------------------
# The only trustworthy answer is an empirical one. Static reasoning about the
# signer -- "SSH_AUTH_SOCK is set, so the agent must be reachable" -- is wrong for
# this repository: gpg.ssh.program is op-ssh-sign, which reaches the 1Password app
# over its own IPC and ignores SSH_AUTH_SOCK entirely. Signing here succeeds with
# SSH_AUTH_SOCK unset and fails with the app locked, which no amount of inspecting
# the environment would reveal.
#
# So: replay this repository's resolved signing configuration in a throwaway
# repository and sign a real commit with it. Whatever breaks for the probe is
# exactly what would have broken for the commit about to be created.
#
# Cost is roughly 0.15s. With op-ssh-sign it can surface one extra 1Password
# authorization at the start of a session; that prompt IS the check working, and
# it appears before the commit rather than after the merge gate.
PROBE_DIR=$(mktemp -d)
cleanup() { rm -rf "$PROBE_DIR"; }
trap cleanup EXIT

# An empty hooks path. Without it, a globally-configured core.hooksPath would make
# the probe commit re-enter this very hook.
mkdir -p "$PROBE_DIR/nohooks" "$PROBE_DIR/repo"

# CRITICAL: run the probe with NO inherited git environment.
#
# git exports GIT_INDEX_FILE (and GIT_PREFIX, GIT_AUTHOR_*, ...) into its hooks.
# Those variables outrank directory-based discovery, so a probe invoked as
# `git -C <probe-repo> commit` still picks up the REAL repository's index -- and
# commits the developer's actual staged files, under the probe's message, onto
# the real branch. Observed while building this check: the probe silently
# committed the entire staged changeset as "signing probe".
#
# So: strip the inherited GIT_* variables, then point GIT_DIR, GIT_WORK_TREE and
# GIT_INDEX_FILE at the probe repository by ABSOLUTE path.
#
# But "strip everything GIT_*" is too blunt, because that namespace holds two
# unrelated kinds of variable and this check needs opposite things from them:
#
#   LOCATION vars (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_PREFIX, ...)
#       decide WHICH REPOSITORY git touches. These are the contamination vector,
#       and they must be stripped and re-pinned. ISOLATION.
#
#   CONFIG-RESOLUTION vars (GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, GIT_CONFIG_COUNT,
#       GIT_CONFIG_KEY_*/VALUE_*, GIT_CONFIG_PARAMETERS) decide WHICH SETTINGS git
#       reads. These must be PRESERVED: the probe's entire job is to reproduce the
#       caller's resolved signing configuration, and a caller who masked their
#       global config must see a probe that is masked the same way. FIDELITY.
#
# Stripping the second group is a real defect, not a theoretical one: with
# GIT_CONFIG_GLOBAL=/dev/null and gpg.ssh.program unset in the repo, this script
# reported `gpg.ssh.program = (unset)` while the probe went on to invoke
# op-ssh-sign out of the masked ~/.gitconfig. The probe was answering a question
# about a configuration the caller was not using.
#
# Preserving them does not reopen the contamination hole. The location vars are
# still stripped, and the explicit GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE below
# outrank anything a config file or GIT_CONFIG_PARAMETERS could say about where
# the repository lives (core.worktree, core.hooksPath). Isolation is enforced by
# the location pinning; the config vars only affect which settings are read.
#
# GIT_EXEC_PATH is kept for a third reason -- it locates git's own subcommands,
# and dropping it can break git entirely.
#
# The cleaning must cover `git init` too, not only the commit: with an inherited
# GIT_DIR, `git init <path>` initializes into GIT_DIR and ignores <path>, so the
# probe repository is never created and the probe fails with a confusing "not a
# git repository".
CLEAN_ENV=(env)
while IFS='=' read -r name _; do
    case "$name" in
        GIT_EXEC_PATH) ;;
        GIT_CONFIG_GLOBAL | GIT_CONFIG_SYSTEM | GIT_CONFIG_NOSYSTEM) ;;
        GIT_CONFIG_COUNT | GIT_CONFIG_KEY_* | GIT_CONFIG_VALUE_*) ;;
        GIT_CONFIG_PARAMETERS) ;;
        GIT_*) CLEAN_ENV+=(-u "$name") ;;
    esac
done < <(env)

"${CLEAN_ENV[@]}" git init -q "$PROBE_DIR/repo"

PROBE_ENV=(
    "${CLEAN_ENV[@]}"
    "GIT_DIR=$PROBE_DIR/repo/.git"
    "GIT_WORK_TREE=$PROBE_DIR/repo"
    "GIT_INDEX_FILE=$PROBE_DIR/repo/.git/index"
)

# Carry over every config key that participates in signing. Values are read from
# THIS repository, so local-over-global precedence is already resolved.
#
# This does NOT by itself pin the probe's configuration, and an earlier version of
# this comment wrongly claimed it did. Only NON-EMPTY values are carried, so a key
# the caller resolves as UNSET is passed as nothing at all -- and the probe then
# falls back to whatever its own config resolution turns up for it. Absence is not
# transmissible this way. What actually makes the probe faithful is preserving the
# caller's GIT_CONFIG_* variables above, so that its fallback resolves against the
# same files the caller used. This list sits on top of that: it pins the values
# that repo-local config overrides, which the env vars alone would not convey.
PROBE_ARGS=(-C "$PROBE_DIR/repo" -c "core.hooksPath=$PROBE_DIR/nohooks")
for key in gpg.format user.signingkey gpg.program gpg.ssh.program \
    gpg.ssh.defaultKeyCommand gpg.openpgp.program gpg.x509.program; do
    value=$(git config --get "$key" 2>/dev/null || true)
    [ -n "$value" ] && PROBE_ARGS+=(-c "$key=$value")
done
# Identity is irrelevant to the signature but git refuses to commit without one,
# and the developer's own may be unset in a bare environment.
PROBE_ARGS+=(-c "user.name=signing probe" -c "user.email=probe@localhost")

set +e
PROBE_ERR=$("${PROBE_ENV[@]}" git "${PROBE_ARGS[@]}" commit --quiet --allow-empty -S -m "signing probe" 2>&1)
PROBE_STATUS=$?
set -e

if [ "$PROBE_STATUS" -ne 0 ]; then
    fail "the configured commit signer is unreachable or failed." \
        "git reported:" \
        "" \
        "$PROBE_ERR"
fi

# Verify against the RAW COMMIT OBJECT, never `git log --format=%G?`.
#
# %G? reports N -- "no signature" -- for a genuinely signed commit whenever
# gpg.ssh.allowedSignersFile is unset, because it is answering "can I VERIFY this
# signature", not "is there one". That is the default state on a fresh clone, so a
# check built on %G? would fail every correctly signed commit. Observed directly
# during the #2624 recovery and pinned by a test in
# scripts/test-check-commit-signing.sh.
#
# The commit header block is everything up to the first blank line; a `gpgsig`
# header there is the signature itself, for both openpgp and ssh formats.
if ! "${PROBE_ENV[@]}" git -C "$PROBE_DIR/repo" cat-file commit HEAD |
    sed -n '1,/^$/p' | grep -q '^gpgsig'; then
    fail "the signer reported success but produced a commit with no signature." \
        "This is the silent-unsigned-commit failure mode from #2624."
fi

echo -e "${GREEN}PASS${RESET} signed-commits (signer verified, signature present on probe commit)"
