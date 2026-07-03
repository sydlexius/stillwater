#!/usr/bin/env python3
"""prefs-coverage.py -- .prefs.toml UI-preference coverage gate (Phase 2, #201).

Vendored into scripts/ (#2195) and invoked as a step in
scripts/pre-push-gate.sh, the command .gates.toml's [prep_pr] gate runs (also
run by the git pre-push hook and CI). This is Layer 1 only: it enforces that
a changed pref-bearing surface does not REGRESS -- i.e. drop a driving
token/class it previously carried. It does NOT catch a CSS cascade-override
(a more specific rule beating a pref-driven var) -- that needs a rendered
getComputedStyle assertion (Layer 2, tracked separately).

Reads `.prefs.toml` at the repo root (schema:
skills/orchestrate/templates/prefs.toml.md). For each `[[pref]]`, this checks
the DIRECTLY-CHANGED surfaces (`git diff BASE..HEAD`, BASE resolved like
patch-coverage.sh) that match the pref's `surface` glob. It is
REGRESSION-ONLY: a matching changed file is flagged MISSING only when its
BASE-revision content matched the pref's `verify` regex and its HEAD-revision
content no longer does -- a surface that used to honor the pref and stopped.
A surface that never carried the token (including a brand-new file, which has
no BASE revision) is not required to carry it and is not flagged; a broad
`surface` glob (e.g. `web/templates/*.templ`, which via fnmatch's `*`
matching across `/` reaches every template in the tree) therefore does not
hard-fail routine edits to templates that were never wired into a given pref.
An un-exempted regression is a HARD failure UNLESS the surface+pref is
covered by an `[[exempt]]` block (printed with its reason). This is the
"necessary" static check; the adversarial-review charter's rendered
Playwright pass is the "sufficient" one.

Design decisions (from the #200/#201 brainstorm, refined post-#2195 to fix a
false-positive hard-fail on unrelated template edits): uniform hard-gate +
per-surface opt-out; narrow surface = directly-changed files;
regression-only (never require a surface to newly adopt a pref token);
verify is necessary-not-sufficient; absent-manifest self-skip.

Exit codes: 0 = pass, self-skip, or nothing-in-scope; 1 = un-exempted
regression (a surface dropped a pref token it had at base); 2 = config /
parse / setup error, including any git failure (fails CLOSED -- a git error
must never degrade to a silent PASS).
"""
import sys
import os
import re
import subprocess
import fnmatch

try:
    import tomllib
except ModuleNotFoundError:  # pragma: no cover - Python < 3.11
    print("prefs-coverage: requires Python 3.11+ (tomllib).", file=sys.stderr)
    sys.exit(2)


class GitError(Exception):
    """A git subprocess errored or timed out.

    This is a GATE whose whole job is to catch a regression; silently treating
    a git failure as "nothing changed" or "file absent at base" would let a
    real regression slip through (fail OPEN). So any unexpected git failure is
    surfaced as this exception and the caller must FAIL CLOSED (exit 2 --
    setup/config error), never degrade to a PASS.
    """


def sh(args, timeout=120):
    # Bounded so a hung git op fails fast (rc 124) instead of burning the job budget.
    try:
        return subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(args, 124, "", f"timed out after {timeout}s")


def resolve_base():
    # Honor an explicit BASE override first, same env var patch-coverage.sh
    # reads. pre-push-gate.sh forwards its own already-validated $BASE (the
    # merge-base SHA it computed and rev-parse-verified) so that in a
    # shallow-clone / CI context where `origin/main` is not fetched, the gate
    # and this script agree on the diff base instead of this script's ladder
    # silently diverging. A caller (CI matrix, ad-hoc local run) can likewise
    # pin BASE explicitly.
    env_base = os.environ.get("BASE", "").strip()
    if env_base:
        if sh(["git", "rev-parse", "--verify", "-q", env_base]).returncode == 0:
            return env_base
        # An EXPLICIT BASE that doesn't resolve is a setup error, not something
        # to paper over by silently falling through to origin/main (which could
        # widen or narrow the diff without the caller knowing). Fail CLOSED.
        raise GitError(f"explicit BASE {env_base!r} does not resolve to a git object")
    # Mirror patch-coverage.sh's BASE fallback chain. If NONE of these refs exist
    # (fresh / unrelated history), returns None -> changed_files falls back to
    # `git diff HEAD` (working-tree only), so the gate fails OPEN rather than
    # treating the whole tree as changed. Intentional, matching patch-coverage.sh.
    for ref in ("origin/main", "main", "origin/master", "master"):
        if sh(["git", "rev-parse", "--verify", "-q", ref]).returncode == 0:
            return ref
    return None


def changed_files(base):
    # Returns (entries, base_sha) where each entry is (head_path, base_path):
    #   head_path -- the path at HEAD (matched against the surface glob and
    #                read for the HEAD-revision content).
    #   base_path -- the path to read BASE-revision content from, or None when
    #                the file has no base (an Added file). For a Rename this is
    #                the OLD path, so a rename+edit that drops a token is still
    #                compared against the token it had under its former name.
    # base_sha is the merge-base commit the diff ranges against (None when
    # there's no usable base -- see resolve_base's fail-open comment).
    #
    # FAIL CLOSED on any git error: a git diff that times out or errors must
    # NOT degrade to "0 files changed" (a false PASS that suppresses the whole
    # gate). Only a clean rc==0 with empty output is a legitimate "no changes".
    if base:
        mb_res = sh(["git", "merge-base", base, "HEAD"])
        # rc 1 == no common ancestor (unrelated histories): legitimate, fall
        # back to a working-tree diff. rc 124 (timeout) / 128 (bad rev) / etc.
        # are real failures -> fail closed.
        if mb_res.returncode not in (0, 1):
            raise GitError(f"git merge-base {base} HEAD failed "
                           f"(rc={mb_res.returncode}): {mb_res.stderr.strip()}")
        mb = mb_res.stdout.strip()
        rng = f"{mb}..HEAD" if mb else "HEAD"
        base_sha = mb or None
    else:
        rng = "HEAD"
        base_sha = None
    # --name-status -M: get the change status + old/new paths so a Rename (R)
    # maps base<-old, head<-new. --diff-filter=AMR drops Deletions (a removed
    # file can't regress a token) and Copies. core.quotePath=false keeps
    # non-ASCII paths literal so the downstream os.path.isfile / git show
    # resolve the real filename instead of a C-quoted octal escape.
    res = sh(["git", "-c", "core.quotePath=false", "diff",
              "--name-status", "-M", "--diff-filter=AMR", rng])
    if res.returncode != 0:
        raise GitError(f"git diff --name-status {rng} failed "
                       f"(rc={res.returncode}): {res.stderr.strip()}")
    entries = []
    for line in res.stdout.splitlines():
        if not line.strip():
            continue
        parts = line.split("\t")
        code = parts[0][:1]
        if code == "R" and len(parts) >= 3:
            old_path, new_path = parts[1], parts[2]
            entries.append((new_path, old_path))
        elif code == "A" and len(parts) >= 2:
            entries.append((parts[1], None))          # added -- no base revision
        elif code == "M" and len(parts) >= 2:
            entries.append((parts[1], parts[1]))      # modified -- same path both sides
    return entries, base_sha


def base_content(base_sha, path):
    # Returns the file's content at base_sha, or None if it did not exist
    # there (an Added file, or the pre-rename name -- never a regression, so
    # callers treat None as "no prior state to regress from"). Decodes
    # leniently (errors="replace"), matching the HEAD-content read below -- a
    # stray non-UTF8 byte must not crash the gate.
    #
    # A git-show TIMEOUT or unexpected ERROR must NOT be conflated with
    # "absent at base": doing so silently suppresses a real regression. Only a
    # genuine object-not-found (git's "does not exist in <rev>") is the
    # non-regression None case; anything else raises GitError -> fail closed.
    if not base_sha or path is None:
        return None
    try:
        res = subprocess.run(["git", "show", f"{base_sha}:{path}"],
                              capture_output=True, timeout=120)
    except subprocess.TimeoutExpired:
        raise GitError(f"git show {base_sha}:{path} timed out after 120s")
    if res.returncode == 0:
        return res.stdout.decode("utf-8", errors="replace")
    stderr = res.stderr.decode("utf-8", errors="replace")
    low = stderr.lower()
    # git reports a path absent from a tree with one of these fatals. But the
    # SAME "exists on disk, but not in <rev>" message is also emitted for a
    # bogus/unfetched base rev (a 40-hex object git can't resolve) -- so the
    # message alone cannot tell "file legitimately absent at a real base"
    # (benign -> None) from "base rev is garbage" (must fail closed). Confirm
    # base_sha actually resolves to a commit before trusting the benign path;
    # otherwise a bad base would silently suppress every regression.
    if "does not exist in" in low or "exists on disk, but not in" in low:
        if sh(["git", "rev-parse", "--verify", "-q", f"{base_sha}^{{commit}}"]).returncode == 0:
            return None  # path genuinely did not exist at a valid base -- not a regression
        raise GitError(f"base rev {base_sha!r} does not resolve to a commit "
                       f"(git show reported: {stderr.strip()})")
    raise GitError(f"git show {base_sha}:{path} failed "
                   f"(rc={res.returncode}): {stderr.strip()}")


def repo_root():
    # Resolve the repo root so every later git op anchors there. A git failure
    # here must FAIL CLOSED (GitError -> exit 2), never fall back to os.getcwd():
    # a wrong root would run the gate against the wrong tree, and a self-skip
    # (no .prefs.toml found at the bogus root) would silently suppress every
    # regression -- exactly the fail-open a fail-closed gate must not have.
    res = sh(["git", "rev-parse", "--show-toplevel"])
    if res.returncode != 0:
        raise GitError(f"git rev-parse --show-toplevel failed "
                       f"(rc={res.returncode}): {res.stderr.strip()}")
    root = res.stdout.strip()
    if not root:
        raise GitError("git rev-parse --show-toplevel returned empty output")
    return root


def main():
    try:
        root = repo_root()
    except GitError as e:
        print(f"prefs-coverage: SETUP -- git failure resolving repo root, "
              f"failing closed: {e}", file=sys.stderr)
        return 2
    os.chdir(root)  # anchor all subsequent git ops to the repo root (robust from nested cwd)
    manifest = os.path.join(root, ".prefs.toml")
    if not os.path.isfile(manifest):
        print("prefs-coverage: no .prefs.toml -- self-skip (no UI-preference manifest).")
        return 0
    try:
        with open(manifest, "rb") as fh:
            cfg = tomllib.load(fh)
    except (tomllib.TOMLDecodeError, OSError) as e:
        print(f"prefs-coverage: .prefs.toml parse error: {e}", file=sys.stderr)
        return 2

    # Fail CLOSED on malformed-but-valid TOML shapes. A raw crash would exit 1 and
    # masquerade as a MISSING finding; config/shape errors must be a clean exit 2.
    prefs = cfg.get("pref", [])
    exempts = cfg.get("exempt", [])
    if not isinstance(prefs, list) or not all(isinstance(p, dict) for p in prefs):
        print("prefs-coverage: CONFIG -- [[pref]] must be an array of tables.", file=sys.stderr)
        return 2
    if not isinstance(exempts, list) or not all(isinstance(e, dict) for e in exempts):
        print("prefs-coverage: CONFIG -- [[exempt]] must be an array of tables.", file=sys.stderr)
        return 2
    if not prefs:
        print("prefs-coverage: .prefs.toml has no [[pref]] entries -- nothing to check.")
        return 0

    # NOTE: [source] is a human-read pointer only (file/docs) -- this tool does NOT
    # execute anything from it. A key-DRIFT check that ran the repo's key-emitting
    # command was deliberately dropped (#201): executing any PR-controlled command in
    # CI is an RCE vector, and the drift check is optional; it can return later behind
    # a trusted-context gate. The enumeration-from-authoritative-source discipline
    # stays a reviewer concern (the adversarial-review charter), not a shell exec here.
    try:
        changed, base_sha = changed_files(resolve_base())
    except GitError as e:
        print(f"prefs-coverage: SETUP -- git failure, failing closed: {e}", file=sys.stderr)
        return 2
    if not changed:
        print("prefs-coverage: no changed files in range -- nothing to check.")
        return 0

    # Normalize/validate [[exempt]].prefs up front. TOML allows a bare string, and
    # `key in "font_family"` is SUBSTRING membership -- it would falsely exempt a pref
    # named "font". Coerce a lone string to a 1-element list; filter a list to strings;
    # reject any other type as a CONFIG error (fail closed).
    norm_exempts = []
    for ex in exempts:
        pv = ex.get("prefs", [])
        if isinstance(pv, str):
            pv = [pv]
        elif isinstance(pv, list):
            pv = [x for x in pv if isinstance(x, str)]
        else:
            print(f"prefs-coverage: CONFIG -- [[exempt]].prefs must be a string or "
                  f"array of strings: {ex}", file=sys.stderr)
            return 2
        norm_exempts.append((ex.get("surface", ""), pv, ex.get("reason", "(no reason given)")))

    def exemption(path, key):
        for glob, pv, reason in norm_exempts:
            if glob and fnmatch.fnmatch(path, glob) and key in pv:
                return reason
        return None

    missing = []
    honored = 0
    for p in prefs:
        key, surf, verify = p.get("key"), p.get("surface"), p.get("verify")
        if not (key and surf and verify):
            print(f"prefs-coverage: CONFIG -- each [[pref]] needs key + surface + verify: {p}",
                  file=sys.stderr)
            return 2
        try:
            rx = re.compile(verify)
        except re.error as e:
            print(f"prefs-coverage: CONFIG -- bad verify regex for pref {key!r}: {e}",
                  file=sys.stderr)
            return 2
        for head_path, base_path in changed:
            if not fnmatch.fnmatch(head_path, surf):
                continue
            full = os.path.join(root, head_path)
            if not os.path.isfile(full):
                continue  # deleted/renamed-away in the range
            try:
                with open(full, encoding="utf-8", errors="replace") as fh:
                    head_body = fh.read()
            except OSError as e:
                # TOCTOU: the file passed os.path.isfile but vanished / became
                # unreadable before this read (deleted, renamed, or a
                # permission error between the diff and now). A file that
                # isn't there can't be regressing a token -- SKIP with a
                # warning rather than crashing the gate (exit 1 would
                # masquerade as a MISSING finding).
                print(f"  [SKIP   ] {head_path}  ({key}) -- unreadable ({e}); "
                      "not treated as a regression", file=sys.stderr)
                continue
            has = bool(rx.search(head_body))

            # REGRESSION-ONLY: a surface is only required to keep a token it
            # already had. base_content() returns None for a file with no base
            # revision (a brand-new/Added file, or a rename's former name that
            # is looked up under base_path) -- that is never a regression, so
            # it's treated the same as "never carried the token" below (skip,
            # no flag). A git error while reading base content raises GitError
            # and fails the whole gate closed (exit 2).
            try:
                base_body = base_content(base_sha, base_path)
            except GitError as e:
                print(f"prefs-coverage: SETUP -- git failure reading base content, "
                      f"failing closed: {e}", file=sys.stderr)
                return 2
            had = bool(base_body is not None and rx.search(base_body))

            if has:
                honored += 1
                print(f"  [HONORS ] {head_path}  ({key})")
            elif had:
                # Dropped a token the file had at base -- a genuine regression.
                reason = exemption(head_path, key)
                if reason is not None:
                    print(f"  [EXEMPT ] {head_path}  ({key}) -- {reason}")
                else:
                    missing.append((head_path, key))
                    print(f"  [MISSING] {head_path}  ({key}) -- dropped /{verify}/ present at base")
            else:
                # Never carried the token (incl. brand-new files) -- not a
                # regression, nothing to flag.
                print(f"  [OK     ] {head_path}  ({key}) -- never carried /{verify}/, no regression")

    if missing:
        print(f"\nprefs-coverage: {len(missing)} un-exempted regression(s) (surface x pref "
              "dropped a token it had at base). Restore the pref wiring, or add an "
              "[[exempt]] block with a reason.",
              file=sys.stderr)
        return 1
    print(f"\nprefs-coverage: OK ({honored} honored, no un-exempted misses).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
