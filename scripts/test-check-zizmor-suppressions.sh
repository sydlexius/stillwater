#!/usr/bin/env bash
# test-check-zizmor-suppressions.sh -- hermetic mutation tests for
# check-zizmor-suppressions.sh.
#
# WHY THIS EXISTS. The guard's first implementation parsed YAML with awk and
# passed every hand-run smoke test its author tried. Adversarial review then
# found two BYPASSES (a directive on a child line of the `on:` block; a quoted
# trigger key) and one false positive (a directive above `on:`, which zizmor
# does not honor). A guard for a security suppression that quietly returns OK is
# worse than no guard, because it manufactures confidence.
#
# So every one of those cases is pinned here. The tests build throwaway repos in
# a temp dir -- nothing touches the real workflows.
#
# The BYPASS cases are the ones that matter: each is a workflow that genuinely
# suppresses the audit AND genuinely carries a dangerous trigger. Every one was
# verified against zizmor 1.28.0 by hand as really-suppressed at the time of
# writing; this harness asserts the GUARD catches them, which is the part that
# can regress.
#
# Run: bash scripts/test-check-zizmor-suppressions.sh

set -uo pipefail

GUARD="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-zizmor-suppressions.sh"

if [ ! -f "$GUARD" ]; then
  echo "FATAL: cannot find check-zizmor-suppressions.sh next to this test" >&2
  exit 1
fi

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass=0
fail=0

# A workflow body that satisfies the manifest, used as the baseline the
# mutations are applied to. Mirrors the real files' shape closely enough that
# the guard's parsing path is the same one production takes.
write_baseline() {
  local root="$1"
  mkdir -p "$root/.github/workflows" "$root/scripts"
  cp "$GUARD" "$root/scripts/check-zizmor-suppressions.sh"

  cat > "$root/.github/workflows/dependabot-merge.yml" <<'YEOF'
name: Merge approved Dependabot PRs
on: # zizmor: ignore[dangerous-triggers] justified
  workflow_run:
    workflows: ["CI"]
    types:
      - completed
  push:
    branches:
      - main
permissions: {}
jobs:
  merge:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF

  cat > "$root/.github/workflows/pr-labels.yml" <<'YEOF'
name: Label gate
on: # zizmor: ignore[dangerous-triggers] justified
  pull_request_target:
    types: [opened]
    branches: [main]
permissions: {}
jobs:
  require-label:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF

  cat > "$root/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on: # zizmor: ignore[dangerous-triggers] justified
  pull_request_target:
    types: [opened, edited]
    branches: [main]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# run_case <name> <expected-exit> <mutator-fn> [expected-output-substring]
#
# The optional 4th argument asserts on the guard's OUTPUT, not just its exit
# code. Exit-code-only assertions pass on a guard that fails for the WRONG
# reason -- including one that dies on a traceback -- so any case whose point is
# a specific detection path pins that path's message.
run_case() {
  local name="$1" expect="$2" mutate="$3" expect_out="${4:-}"
  local slug root
  slug="$(echo "$name" | tr -c 'a-zA-Z0-9' '_')"
  root="$TMPROOT/$slug"
  rm -rf "$root"
  write_baseline "$root"
  "$mutate" "$root"

  local out rc
  out="$(cd "$root" && bash scripts/check-zizmor-suppressions.sh 2>&1)"
  rc=$?

  if [ "$rc" -ne "$expect" ]; then
    echo "  FAIL  $name -- expected exit $expect, got $rc"
    echo "$out" | sed 's/^/          /'
    fail=$((fail + 1))
    return
  fi

  # A traceback means the guard crashed rather than decided. It still exits
  # non-zero, so an exit-code-only check would call that a pass.
  if echo "$out" | grep -q 'Traceback (most recent call last)'; then
    echo "  FAIL  $name -- guard crashed (traceback) rather than reporting"
    echo "$out" | sed 's/^/          /'
    fail=$((fail + 1))
    return
  fi

  if [ -n "$expect_out" ] && ! echo "$out" | grep -qF "$expect_out"; then
    echo "  FAIL  $name -- exit $rc as expected, but output lacked: $expect_out"
    echo "$out" | sed 's/^/          /'
    fail=$((fail + 1))
    return
  fi

  echo "  PASS  $name (exit $rc)"
  pass=$((pass + 1))
}

noop() { :; }

# --- baseline -------------------------------------------------------------

mut_clean() { :; }

# --- BYPASS cases (guard MUST fail; each really suppresses the audit) ------

# Directive on a CHILD line of the on: block. zizmor honors this -- the finding
# spans the whole block -- so the audit is off while the block looks innocent.
mut_directive_on_child_line() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: Sneaky
on:
  pull_request_target:
    types: [opened] # zizmor: ignore[dangerous-triggers] looks fine
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Same, with the directive on the trigger key itself.
mut_directive_on_trigger_key() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: Sneaky2
on:
  pull_request_target: # zizmor: ignore[dangerous-triggers]
    types: [opened]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# A QUOTED trigger key added to an already-manifested file. The old regex-based
# key match dropped it, so the widening was invisible.
mut_quoted_trigger_key() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on: # zizmor: ignore[dangerous-triggers] justified
  "workflow_run":
    workflows: ["CI"]
    types: [completed]
  pull_request_target:
    types: [opened, edited]
    branches: [main]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

mut_single_quoted_trigger_key() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on: # zizmor: ignore[dangerous-triggers] justified
  'workflow_run':
    workflows: ["CI"]
    types: [completed]
  pull_request_target:
    types: [opened, edited]
    branches: [main]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Flow style. A line-oriented parser reports "(none)" here -- right answer by
# accident, or wrong answer entirely, depending on the block.
mut_flow_style() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on: {pull_request_target: {types: [opened]}, workflow_run: {workflows: ["CI"]}} # zizmor: ignore[dangerous-triggers]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Sequence form: `on: [a, b]`.
mut_sequence_form() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: SeqForm
on: [push, pull_request_target] # zizmor: ignore[dangerous-triggers]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# The plain widening the guard was built for.
mut_added_dangerous_trigger() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on: # zizmor: ignore[dangerous-triggers] justified
  pull_request_target:
    types: [opened, edited]
    branches: [main]
  workflow_run:
    workflows: ["CI"]
    types: [completed]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Child-line directive on an ALREADY-MANIFESTED file, widened with a second
# dangerous trigger. This is the case that actually pins the SPAN logic: the
# unmanifested variants above would still fail (via the manifest branch) even if
# span detection regressed to "the `on:` line only", so on their own they anchor
# the wrong thing. Here the file IS manifested, so the guard only reports if it
# both detects the child-line directive AND compares the trigger set.
mut_child_line_directive_manifested() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on:
  pull_request_target:
    types: [opened, edited] # zizmor: ignore[dangerous-triggers] justified
    branches: [main]
  workflow_run:
    workflows: ["CI"]
    types: [completed]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Same idea with the directive on the trigger key of a manifested file.
mut_trigger_key_directive_manifested() {
  cat > "$1/.github/workflows/pr-milestone.yml" <<'YEOF'
name: PR Milestone
on:
  pull_request_target: # zizmor: ignore[dangerous-triggers] justified
    types: [opened, edited]
  workflow_run:
    workflows: ["CI"]
    types: [completed]
permissions: {}
jobs:
  assign-milestone:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# A stray non-mapping YAML file in the workflows directory must produce a
# readable decision, never a Python traceback.
mut_non_mapping_yaml() {
  cat > "$1/.github/workflows/zz-seq.yml" <<'YEOF'
- this file is a top-level sequence
- not a workflow mapping
YEOF
}

# A suppressing file absent from the manifest.
mut_unmanifested_file() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: Unmanifested
on: # zizmor: ignore[dangerous-triggers]
  pull_request_target:
    types: [opened]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# A manifest entry whose file no longer suppresses.
mut_manifest_rot() {
  sed -i.bak 's/ # zizmor: ignore\[dangerous-triggers\] justified//' \
    "$1/.github/workflows/pr-labels.yml"
  rm -f "$1/.github/workflows/pr-labels.yml.bak"
}

# Multi-rule directive; dangerous-triggers is one of several.
mut_multi_rule_directive() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: MultiRule
on: # zizmor: ignore[artipacked,dangerous-triggers] several
  pull_request_target:
    types: [opened]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# --- FALSE-POSITIVE cases (guard MUST pass) -------------------------------

# Directive ABOVE `on:`. zizmor does NOT honor this -- it is outside the
# finding's span -- so the file suppresses nothing and must not be treated as a
# suppression.
mut_directive_above_on() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: NotSuppressed
# zizmor: ignore[dangerous-triggers] this placement does nothing
on:
  push:
    branches: [main]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# Prose QUOTING the directive, as gate.yml's own failure message does. Must not
# be mistaken for a suppression -- a guard that trips on documentation about
# itself trains people to appease it.
mut_directive_in_prose() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: Prose
on:
  push:
    branches: [main]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - name: Explain
        run: |
          echo "A workflow's \`# zizmor: ignore[dangerous-triggers]\` suppresses"
          echo "the audit for its WHOLE on: block."
YEOF
}

# A different rule's directive must not register as a dangerous-triggers one.
mut_other_rule_directive() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: OtherRule
on: # zizmor: ignore[template-injection]
  push:
    branches: [main]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

# A non-suppressed workflow carrying a dangerous trigger is NOT this guard's
# business -- zizmor itself reports that one, loudly.
mut_unsuppressed_dangerous_trigger() {
  cat > "$1/.github/workflows/zz-new.yml" <<'YEOF'
name: Honest
on:
  pull_request_target:
    types: [opened]
permissions: {}
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YEOF
}

echo "check-zizmor-suppressions mutation tests"
echo

echo "baseline:"
run_case "unmutated tree passes" 0 mut_clean

echo
echo "bypass cases (guard must FAIL these):"
run_case "directive on a child line of on:"      1 mut_directive_on_child_line
run_case "directive on the trigger key"          1 mut_directive_on_trigger_key
run_case "quoted trigger key"                    1 mut_quoted_trigger_key    "ADDED:    workflow_run"
run_case "single-quoted trigger key"             1 mut_single_quoted_trigger_key "ADDED:    workflow_run"
run_case "flow-style on: block"                  1 mut_flow_style           "ADDED:    workflow_run"
run_case "sequence-form on: block"               1 mut_sequence_form
run_case "added dangerous trigger"               1 mut_added_dangerous_trigger "ADDED:    workflow_run"
run_case "unmanifested suppressing file"         1 mut_unmanifested_file      "not in the manifest"
run_case "manifest entry no longer suppresses"   1 mut_manifest_rot           "no longer carries"
run_case "multi-rule directive"                  1 mut_multi_rule_directive

echo
echo "span-anchored cases (manifested file -- these pin the span logic itself):"
# Asserting ADDED: is what makes these non-vacuous. They fail ONLY if the guard
# both saw the child-line directive and compared the trigger set -- the manifest
# branch cannot rescue them, because the file is in the manifest.
run_case "child-line directive, manifested"      1 mut_child_line_directive_manifested  "ADDED:    workflow_run"
run_case "trigger-key directive, manifested"     1 mut_trigger_key_directive_manifested "ADDED:    workflow_run"

echo
echo "false-positive cases (guard must PASS these):"
run_case "directive above on: is not honored"    0 mut_directive_above_on
run_case "directive quoted in prose"             0 mut_directive_in_prose
run_case "directive for a different rule"        0 mut_other_rule_directive
run_case "unsuppressed dangerous trigger"        0 mut_unsuppressed_dangerous_trigger
run_case "non-mapping yaml does not crash"       0 mut_non_mapping_yaml

echo
echo "passed: $pass   failed: $fail"
[ "$fail" -eq 0 ] || exit 1
