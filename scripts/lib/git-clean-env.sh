#!/usr/bin/env bash
#
# git-clean-env.sh -- strip an inherited git environment before running git
# against a throwaway fixture repository (#3051).
#
# `git init <path>` does not always initialize <path>. With GIT_DIR set, git
# resolves it BEFORE it honors the path argument or `-C`, re-initializes the
# directory GIT_DIR names, and ignores what it was asked for. The tell is
# `warning: re-init: ignored --initial-branch=...`.
#
# Git hooks export GIT_DIR and the pre-push gate runs helpers that build fixture
# repos. From a worktree, GIT_DIR names `.git/worktrees/<name>` -- and a worktree
# SHARES the main repository's `.git/config`, so the stray re-init writes
# `core.bare = true` into the MAIN repo, silently disabling its mass-deletion
# guard. It surfaces days later as an unrelated `git merge --ff-only` failing
# with "must be run in a work tree".
#
# WHAT SURVIVES: GIT_EXEC_PATH (locates git's own subcommands; dropping it can
# break git entirely) and the config-resolution vars (GIT_CONFIG_GLOBAL /
# _SYSTEM / _NOSYSTEM / _COUNT / _KEY_* / _VALUE_* / _PARAMETERS), which callers
# set to isolate fixtures from the developer's config -- stripping them would
# UNDO that. Those decide which SETTINGS are read, never WHERE the repository
# is; the LOCATION vars are the contamination vector and are all stripped.
#
# USAGE
#
#   . "$REPO_ROOT/scripts/lib/git-clean-env.sh"
#   git_clean_env_unset                       # strip from THIS shell
#
#   git_clean_env_array                       # or, an `env -u ...` prefix for a
#   "${GIT_CLEAN_ENV[@]}" git init -q "$dir"  # script that must KEEP its own
#                                             # git environment
#
# Prefer git_clean_env_unset: one line, and it cannot be forgotten at the next
# call site. The array form is for a script that also operates on the invoking
# repository.

# _git_clean_env_names prints the name of every GIT_* variable to strip.
_git_clean_env_names() {
    local name _
    while IFS='=' read -r name _; do
        case "$name" in
            GIT_EXEC_PATH) ;;
            GIT_CONFIG_GLOBAL | GIT_CONFIG_SYSTEM | GIT_CONFIG_NOSYSTEM) ;;
            GIT_CONFIG_COUNT | GIT_CONFIG_KEY_* | GIT_CONFIG_VALUE_*) ;;
            GIT_CONFIG_PARAMETERS) ;;
            GIT_*) printf '%s\n' "$name" ;;
        esac
    done < <(env)
}

# git_clean_env_unset removes them from the CURRENT shell.
git_clean_env_unset() {
    local name
    while read -r name; do
        unset "$name"
    done < <(_git_clean_env_names)
}

# git_clean_env_array populates the global GIT_CLEAN_ENV array with an
# `env -u ... -u ...` command prefix. A fixed global rather than a caller-named
# variable: a nameref (`local -n`) needs bash 4.3+, and macOS ships bash 3.2.
git_clean_env_array() {
    local name
    GIT_CLEAN_ENV=(env)
    while read -r name; do
        GIT_CLEAN_ENV+=(-u "$name")
    done < <(_git_clean_env_names)
}
