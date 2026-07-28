#!/usr/bin/env bash
# check-zizmor-suppressions.sh -- assert that no workflow silently widens the
# blast radius of a `# zizmor: ignore[dangerous-triggers]` suppression.
#
# THE PROBLEM THIS EXISTS FOR (#2842, #2843).
#
# Three workflows suppress zizmor's dangerous-triggers audit in-file, because
# each genuinely needs a dangerous trigger and has closed the risk by other
# means (no checkout, no execution of PR-authored content, minimized
# permissions). Each carries a long written justification.
#
# The trap is that zizmor's finding SPANS THE WHOLE `on:` MAPPING, not the
# individual trigger key. So the directive does not mean "forgive workflow_run
# here" -- it means "never report a dangerous trigger anywhere in this block,
# ever". Measured, not assumed: appending a second dangerous trigger to a
# suppressed block yields "No findings to report".
#
# The consequence is a disarmed detector. Someone adds `pull_request_target` to
# a suppressed `on:` block in six months; zizmor stays silent, no code-scanning
# alert is raised, and the post-merge scan reports clean. The workflow now runs
# fork-authored PRs against the base repo's write token with nothing objecting.
# The written justification above it still describes the ORIGINAL trigger and
# reads as though it covers the new one.
#
# A comment cannot enforce that, because comments do not fail builds. This
# script is the enforcement: it pins the exact set of dangerous triggers each
# suppressed block is allowed to contain. Adding one fails the gate until the
# author updates the manifest below, which is the moment they must also justify
# it. The goal is not to forbid the change -- it is to make it impossible to
# make ACCIDENTALLY, and to force the justification to stay in sync with what
# is actually being suppressed.
#
# WHY THE DETECTION IS NOT HAND-ROLLED TEXT MATCHING.
#
# The first version of this script parsed the `on:` block with awk and matched
# the directive by looking at the `on:` line plus the comment run above it. Its
# adversarial review found two guard BYPASSES and one false positive, all from
# that choice, and all of them the kind a real attacker or a distracted author
# hits by accident:
#
#   - A directive on a CHILD line of the block (the trigger key, or even a
#     `types:` line) suppresses the audit exactly as well, and the awk detector
#     did not see it. A workflow could carry `pull_request_target` with the
#     audit fully off and the guard printed OK.
#   - A QUOTED trigger key (`"workflow_run":`) did not match the key regex, so
#     a dangerous trigger added to an already-manifested file was silently
#     dropped from the comparison. Two quote characters defeated the guard.
#   - Conversely a directive in the comment block ABOVE `on:` -- which zizmor
#     does NOT honor, verified -- was reported as a suppression, failing the
#     gate on a file that suppresses nothing.
#
# So: structure comes from a real YAML parser, never a regex over lines. And
# the directive is matched over the FINDING'S SPAN (the `on:` block's own
# lines), which is what zizmor actually honors -- not the line above it, and
# not the whole file. Every case above is covered by a mutation test in
# scripts/test-check-zizmor-suppressions.sh; run it after touching this file.
#
# WHAT COUNTS AS DANGEROUS. zizmor's dangerous-triggers audit flags
# `pull_request_target` and `workflow_run`: both run in the base repository's
# context (base secrets, base write scopes) while being reachable from a fork.
# Every other trigger either cannot be caused by an outsider (push, schedule,
# workflow_dispatch) or runs with a fork-downgraded read-only token
# (pull_request). If zizmor adds another trigger to the audit, add it to
# DANGEROUS_TRIGGERS below -- the failure mode of NOT doing so is a miss, so
# that list errs toward including anything base-context and fork-reachable.
#
# WHY A MANIFEST RATHER THAN "JUST RE-RUN ZIZMOR". Re-running zizmor cannot
# help: the suppression is precisely what makes the new trigger invisible to
# it. A scan of a widened block comes back clean. The only way to detect the
# widening is to compare the block's contents against a declared expectation,
# which is what this does.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if ! command -v python3 >/dev/null 2>&1; then
  echo "check-zizmor-suppressions: python3 not found -- cannot parse workflows." >&2
  echo "  This guard fails CLOSED: it protects a security suppression, so a" >&2
  echo "  missing interpreter must not read as 'no problems found'." >&2
  exit 1
fi

if ! python3 -c 'import yaml' 2>/dev/null; then
  echo "check-zizmor-suppressions: PyYAML not available -- cannot parse workflows." >&2
  echo "  This guard fails CLOSED (see above). Install PyYAML (pip install pyyaml)." >&2
  exit 1
fi

if [ "$#" -gt 0 ]; then
  echo "check-zizmor-suppressions: takes no arguments (got: $*)" >&2
  echo "  It always checks every workflow in .github/workflows/ against the" >&2
  echo "  MANIFEST inside this script. Run it with no arguments." >&2
  exit 2
fi

# Heredoc is single-quoted so the shell expands nothing inside the Python.
python3 - <<'PYEOF'
import glob
import re
import sys

import yaml

# Triggers zizmor's dangerous-triggers audit flags. See the header note.
DANGEROUS_TRIGGERS = {"pull_request_target", "workflow_run"}

# The manifest: for each workflow that suppresses dangerous-triggers, the exact
# set of dangerous triggers its `on:` block is allowed to contain. A file that
# suppresses the audit but is NOT listed here is itself a failure -- that is how
# a newly-suppressed workflow gets forced through review rather than quietly
# inheriting an exemption.
#
# Keep in sync with the justification comment in each file. If you are editing
# this table, you are widening a suppression: say why, in the workflow.
#
# This is the SINGLE source of truth for both directions of the check (a
# suppressing file missing from the manifest, and a manifest entry whose file no
# longer suppresses). An earlier draft restated the file list a second time for
# the staleness loop, which could drift silently.
MANIFEST = {
    ".github/workflows/dependabot-merge.yml": {"workflow_run"},
    ".github/workflows/pr-labels.yml": {"pull_request_target"},
    ".github/workflows/pr-milestone.yml": {"pull_request_target"},
}

DIRECTIVE = re.compile(r"zizmor:\s*ignore\[([^\]]*)\]")


def load(path):
    """Parse a workflow. Returns None if it is not valid YAML."""
    try:
        with open(path, encoding="utf-8") as fh:
            return yaml.safe_load(fh)
    except (yaml.YAMLError, OSError):
        return None


def on_block(doc):
    """The `on:` mapping's value.

    YAML 1.1 resolves a bare `on` key to the boolean True, which is why a
    workflow's triggers are so often found under `True` rather than `"on"`.
    Both spellings are checked, plus the quoted form.
    """
    if not isinstance(doc, dict):
        return None
    for key in (True, "on", "On", "ON"):
        if key in doc:
            return doc[key]
    return None


def triggers_of(block):
    """Every trigger name in an `on:` block, in any of its three legal forms.

    A mapping (`on:\\n  push:`), a sequence (`on: [push, pull_request]`), or a
    bare string (`on: push`). Flow style parses to the same structures, so it
    needs no special handling -- which is the point of using a real parser.
    """
    if isinstance(block, dict):
        return {str(k) for k in block}
    if isinstance(block, list):
        return {str(v) for v in block}
    if isinstance(block, str):
        return {block}
    return set()


def on_block_line_span(path):
    """The 1-based line range of the `on:` block, as (start, end) inclusive.

    Located by re-parsing with a composed node tree so the span comes from the
    parser's own bookkeeping rather than from guessing at indentation.

    The end comes from the value node's end_mark, which for a block mapping
    lands on the FIRST LINE AFTER the block rather than its last line. That is
    deliberately not corrected: it makes the range one line generous, so a
    directive on the line following `on:` is treated as suppressing when zizmor
    would not honor it. That errs toward a spurious gate failure (annoying,
    visible, fixable by moving the comment) rather than toward missing a real
    suppression (a silent bypass, which is the failure this guard exists to
    prevent). Measured: the guard's detected lines are a strict superset of the
    lines zizmor actually honors.
    """
    try:
        with open(path, encoding="utf-8") as fh:
            node = yaml.compose(fh)
    except (yaml.YAMLError, OSError):
        return None
    # Only a mapping can carry an `on:` key. A top-level sequence or bare scalar
    # is not a workflow, and zizmor cannot audit those shapes at all -- so
    # reporting "no suppression here" is both correct and safe. Checked
    # explicitly because iterating a non-mapping node's `.value` raises, and a
    # security gate must not fail by stack trace: a stray .yml in this directory
    # would otherwise turn a clear diagnostic into a traceback.
    if not isinstance(node, yaml.MappingNode):
        return None
    for key_node, value_node in node.value:
        key = getattr(key_node, "value", None)
        # The composer has not applied YAML 1.1 bool resolution yet, so the key
        # is still the literal scalar "on" here.
        if str(key).lower() != "on":
            continue
        start = key_node.start_mark.line + 1
        end = value_node.end_mark.line + 1
        return (start, end)
    return None


def suppresses_dangerous_triggers(path):
    """Whether a dangerous-triggers ignore directive lands in the finding's span.

    zizmor honors the directive when it appears as a YAML comment anywhere
    within the finding's span. For dangerous-triggers that span is the `on:`
    block, so a directive on the `on:` line, on a trigger key, or on any other
    line inside the block all count -- and a directive on the line ABOVE `on:`
    does not (verified against zizmor 1.28.0).
    """
    span = on_block_line_span(path)
    if span is None:
        return False
    start, end = span
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError:
        return False
    for lineno in range(start, min(end, len(lines)) + 1):
        line = lines[lineno - 1]
        # Only a YAML comment carries the directive. Take the text after the
        # first `#`; a `#` inside a quoted scalar would yield prose, which
        # simply will not match the directive pattern.
        hash_at = line.find("#")
        if hash_at == -1:
            continue
        match = DIRECTIVE.search(line[hash_at:])
        if match and "dangerous-triggers" in {
            rule.strip() for rule in match.group(1).split(",")
        }:
            return True
    return False


def main():
    errors = 0
    workflows = sorted(
        set(glob.glob(".github/workflows/*.yml"))
        | set(glob.glob(".github/workflows/*.yaml"))
    )
    if not workflows:
        print("check-zizmor-suppressions: no workflows found -- refusing to pass.")
        print("  Expected .github/workflows/*.yml. Run this from the repo root.")
        return 1

    suppressing = set()

    for path in workflows:
        if not suppresses_dangerous_triggers(path):
            continue
        suppressing.add(path)

        doc = load(path)
        if doc is None:
            print(
                f"::error file={path}::{path} suppresses dangerous-triggers but "
                "could not be parsed as YAML."
            )
            print("  Fix the syntax; this guard cannot verify an unparsable workflow.")
            errors += 1
            continue

        actual = triggers_of(on_block(doc)) & DANGEROUS_TRIGGERS

        if path not in MANIFEST:
            print(
                f"::error file={path}::{path} suppresses zizmor's "
                "dangerous-triggers audit but is not in the manifest in "
                "scripts/check-zizmor-suppressions.sh."
            )
            print(
                "  A suppression hides EVERY dangerous trigger in the file's "
                "`on:` block,"
            )
            print(
                "  not just the one you meant. Add the file to MANIFEST with the "
                "exact"
            )
            print(
                "  triggers it may carry, and justify the suppression in the "
                "workflow itself."
            )
            print(f"  Found: {', '.join(sorted(actual)) or '(none)'}")
            errors += 1
            continue

        expected = MANIFEST[path]
        if actual != expected:
            added = sorted(actual - expected)
            removed = sorted(expected - actual)
            print(
                f"::error file={path}::{path}: dangerous triggers in the "
                "suppressed `on:` block changed."
            )
            print(f"  expected: {', '.join(sorted(expected)) or '(none)'}")
            print(f"  actual:   {', '.join(sorted(actual)) or '(none)'}")
            if added:
                print(f"  ADDED:    {', '.join(added)}")
            if removed:
                print(f"  REMOVED:  {', '.join(removed)}")
            print()
            print(
                "  This file suppresses zizmor's dangerous-triggers audit, so "
                "zizmor will"
            )
            print(
                "  NOT report the change and no code-scanning alert will be "
                "raised. That"
            )
            print("  is why this guard exists.")
            print()
            print(
                "  If the new trigger is intended: justify it in the workflow's "
                "`on:` block"
            )
            print(
                "  comment against the same fork-context reasoning already there "
                "(does it"
            )
            print(
                "  check out PR-head code? does any fork-authored value reach a "
                "shell"
            )
            print(
                "  command, a script, or an API path?), then update MANIFEST in "
                "this script."
            )
            errors += 1

    # The other direction: a manifest entry whose file no longer suppresses, or
    # no longer exists. Left unchecked, the entry would sit there implying a
    # guard that is not actually running.
    for path in sorted(MANIFEST):
        if path not in suppressing:
            print(
                f"::error file=scripts/check-zizmor-suppressions.sh::Manifest "
                f"lists {path}, but that file no longer carries a "
                "dangerous-triggers suppression in its `on:` block."
            )
            print("  Remove it from MANIFEST so the manifest reflects reality.")
            errors += 1

    if errors:
        print()
        print(f"check-zizmor-suppressions: {errors} error(s).")
        return 1

    print(
        f"check-zizmor-suppressions: OK ({len(suppressing)} suppressed "
        "`on:` block(s), all matching the manifest)."
    )
    return 0


sys.exit(main())
PYEOF
