---
description: How Stillwater's rule engine evaluates artists, the three modes per rule, and how fix-all works.
---

<!-- code: internal/rule/model.go (Rule struct, AutomationMode constants, RuleConfig, Violation, FixResult), internal/rule/service.go (defaultRules, filesystemRules, IsFilesystemDependent, DisableFilesystemRules), internal/rule/fixer.go (Pipeline, WriteGate, FixResult Fixed/Dismissed semantics), internal/api/handlers_fix.go (handleFixAll: 409 on concurrent start, FixAllProgress mutex), internal/artist/model.go (DirtySince/RulesEvaluatedAt for incremental run, issue #698) -->
<!-- displaced developer detail: in-memory progress tracker (mutex-protected), 409 status on concurrent fix-all, dirty-subscriber implementation, FilesystemDependent tagging, RulesConsidered diff for pass-persistence (issue #699). Belongs in godoc/architecture-decisions, not in this user page. -->

# Rules

A **rule** is a check Stillwater runs against an artist. It either passes (nothing to do) or it fails -- producing a **violation** with a recommended fix. The rule engine is what converts "your library has 4,000 artists, some need work" into a finite, sortable, fixable list.

## Three modes per rule

Every rule has two independent toggles:

- **Enabled** -- whether the rule runs at all. Disabled rules don't evaluate, don't appear in violation counts, don't surface in the UI's rules tab.
- **Automation mode** -- once enabled, the rule is either **manual** (it finds violations; you decide when to apply fixes) or **auto** (the fixer runs on every detected violation as part of evaluation).

Combined, that's three meaningful states:

| State | What happens |
|---|---|
| Disabled | Rule never evaluates. |
| Enabled, manual | Rule evaluates and surfaces violations. Fixes wait for you to click. |
| Enabled, auto | Rule evaluates and the fixer runs on every detected violation. |

This split (instead of a single tri-state) lets you say "this rule is interesting, keep finding violations, but don't auto-act on it yet" -- the manual mode -- versus "this rule is well-tuned, let it heal the library." Disable when you decide a rule isn't relevant to your collection at all.

The shipped defaults aim for low risk: most rules are enabled in manual mode, a few well-trusted ones default to auto, and a few opinionated rules ship disabled.

<!-- SCREENSHOT: Settings > Rules | state: rules tab showing mix of disabled/manual/auto rules | annotation: the two-toggle pattern -->

## What rules check

Rules fall into three broad categories:

- **NFO** -- the artist's NFO file: presence, MusicBrainz ID present, biography filled in, etc.
- **Image** -- the four image slots: presence, dimensions above the configured threshold, aspect ratio sane, no excessive padding (logos), enough fanart variants.
- **Metadata** -- artist record consistency: directory name matches artist name, IDs not malformed, language of metadata matches the configured preference, etc.

Each rule has a small set of acceptance knobs you can tune -- minimum image dimensions, aspect-ratio tolerance, biography minimum length, article-handling style (prefix / suffix / strip), and so on. The full enumeration lives in the [rules catalogue](../reference/rules-catalogue.md).

## Filesystem-dependent rules

Some rules require an artist directory on disk to evaluate -- the "NFO file exists" rule, for example, has no API equivalent. Those rules are skipped automatically for artists that don't have a directory yet (e.g., pathless artists imported from a connected platform). No false-positive "your NFO doesn't exist" violations on artists that never had a directory in the first place.

If your whole deployment is API-only (rare), there's a one-click action under Settings to disable every filesystem-dependent rule at once.

## How evaluation runs

Two ways to trigger evaluation:

- **Per artist, on demand** -- the artist detail page's "Re-evaluate" action.
- **Bulk, scheduled or manual** -- "Run rules" over a scope (whole library, all artists, a saved view).

The bulk path is **incremental by default**: only artists that have changed since their last evaluation are re-checked. Most runs are fast because most artists are unchanged. If you want a full re-evaluation, the UI exposes a "force" toggle.

Evaluation results persist, so the violation list survives restarts. A passing artist stays passing until something changes (a metadata edit, an image upload, a re-scan).

## The fix step

Each violation is either **fixable** (it has a recommended action) or **detection-only** (you handle it manually). When you trigger a fix -- either manually or because the rule is in auto mode -- the outcome is one of three things:

- **Fixed** -- the violation was repaired. The artist record (and possibly disk) was modified.
- **Dismissed** -- the violation was no longer actionable. Most often because the artist was orphaned (deleted) or no longer matches the rule. The violation is closed without a real change.
- **Still open** -- the fixer ran but couldn't repair (e.g., no candidate image met the rule's threshold). The violation stays open for you to handle.

Before any fix that produces an image or NFO write, Stillwater consults the **conflict gate** -- the same mechanism that powers the "image / NFO writes paused" banner. When a connected platform appears to be actively rewriting files, the gate blocks the write and the fix is deferred. Reactive protection that doesn't depend on whether you've locked anything.

## Fix-all

Sometimes you want to fix every fixable violation in one click. That's "Fix all." Three things to know:

1. **One run at a time** across the whole installation. A second Fix-all while one is running is rejected; the UI tells you to wait.
2. **Async with progress.** The action returns immediately; the UI polls progress (total / processed / fixed / skipped / failed) and renders a bar.
3. **Scope-aware.** You can scope to a library, a saved view, or a single rule's violations.

Behind the scenes each fix is identical to a manual click on a single violation -- same conflict-gate check, same outcome categories. Fix-all is a convenience, not a separate code path.

## What you don't need to think about

- **Persistence.** Rule outcomes survive restarts; re-evaluation after a quiet day is mostly returns from cache.
- **Pathless safety.** Filesystem-dependent rules skip artists without paths.
- **Conflict-gate coordination.** Fixers consult the gate before disk writes; you don't have to pause rules manually when a platform is meddling.
- **Concurrent Fix-all.** One run at a time is enforced for you.

What you do think about: which rules to enable, which ones to trust enough to put on auto, and what to do with the violations that the fixer can't auto-resolve. The rules catalogue is the place to start tuning.
