# Findings: shape, i18n keys, and the rule-engine contract

A *finding* is a single rule violation surfaced to the user. Findings appear on
several screens (Reports rows, artist-detail field chips, the dashboard action
queue, and embedded findings sections). Every surface consumes the same shape,
so surface-specific wording must never leak into the producer (the rule engine).

This document is the contract between the rule engine that emits findings and the
templ layer that renders them.

## Canonical shape

| Field | Required | Rendered as | Constraint |
|---|---|---|---|
| `rule` | yes | Tooltip and link to rule docs only | Dot-or-underscore rule id (e.g. `bio_exists`); never rendered as primary text |
| `severity` | yes | Severity badge | One of `error`, `warning`, `info` |
| `title` | yes | Chip label, list-row title, sticky-header tooltip | Stands alone without `message`; <= 60 chars; truncation on a chip is unacceptable |
| `message` | yes | Drawer body, popover detail | Full sentence naming the concrete values (which provider, which threshold, what was found) |
| `suggestedFix` | yes | Drawer "Fix it" hint | Imperative voice, one sentence, no questions |
| `evidence` | no | Drawer "View source" link | When prefixed `logs:`, drives the error-only Logs deep-link |

The Go producer type is `rule.Violation` (`internal/rule/model.go`); its persisted
form is `rule.RuleViolation`. Today those types carry `RuleID`, `Severity`, and a
fully rendered `Message`. The `title` and `suggestedFix` strings live in i18n (see
below); `message` is still rendered in Go pending the follow-up in
[Migration status](#migration-status).

## i18n key convention

Every built-in rule provides finding strings under a fixed key path in the
translation bundle (`internal/i18n/locales/en.json`):

```
findings.{rule}.title    static label, <= 60 chars, says what is wrong
findings.{rule}.fix      static imperative hint, one sentence
findings.{rule}.message  full sentence with concrete values (see migration status)
```

`{rule}` is the rule id exactly as defined in `internal/rule/service.go` (for
example `findings.bio_exists.title`).

`title` and `fix` are **static** per rule. They take no runtime values, so a
consumer resolves them directly in the templ layer with the existing helper:

```go
title := t(ctx, "findings."+ruleID+".title")
fix   := t(ctx, "findings."+ruleID+".fix")
```

Strings are not baked into Go. Adding a rule without its `title` and `fix` keys
fails the lint test below.

## The 60-char title budget and lint gate

A finding `title` is the label on an artist-detail finding chip. Chips do not
truncate gracefully, so a title must be self-contained and short. The budget is
**60 characters**.

`TestFindingsI18nCoverage` (`internal/rule/findings_i18n_test.go`) enforces the
contract over the embedded bundle:

- every built-in rule has a `findings.{rule}.title` and a `findings.{rule}.fix` key;
- every `findings.{rule}.title` is <= 60 characters.

The test fails the build if a rule is added without its finding strings, or if a
title grows past the budget.

## Rule-engine contract

- The rule id is the stable join key between a finding and its translations. It
  must match the constant in `internal/rule/service.go` and the i18n key segment.
- `severity` is one of `error`, `warning`, `info`.
- Producers do not format `title` or `suggestedFix`; those are resolved from i18n
  at render time so they stay translatable and consistent across surfaces.

## Migration status

This is the state after issue #1342 ("title/fix-first"):

- `findings.{rule}.title` and `findings.{rule}.fix` exist for every built-in rule
  and are resolved in the templ layer.
- `findings.{rule}.message` is **not yet** fully migrated. The human-readable
  `Message` is still built with `fmt.Sprintf` at evaluation time in
  `internal/rule/checkers.go`, `checkers_language.go`, and `checkers_discography.go`,
  and persisted to `rule_violations.violation_message`.

Moving `message` resolution into the templ layer (engine emits the rule code plus
named slot values; the templ layer resolves `findings.{rule}.message` with those
slots) requires named-slot support in `internal/i18n`, which does not exist today.
That work is tracked separately as **#1769** under Milestone 55.5. It needs no
database migration: `rule_violations.violation_message` is a self-healing cache
that `UpsertViolation` overwrites on every rule pass via
`ON CONFLICT(rule_id, artist_id) DO UPDATE`, so existing rows regenerate naturally.
