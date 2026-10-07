---
description: How metadata change history records what triggered a write and what supplied its value, and how to add a write path.
---

# Metadata history

Every tracked field write records a row in `metadata_changes`. A row answers two
independent questions, in two columns.

## The two-column model

| Column | Question | Example |
|---|---|---|
| `source` | What TRIGGERED the write? | `manual`, `scan`, `revert`, `rule:<id>` |
| `producer` | What SUPPLIED the value? | `operator`, `provider:lastfm`, `nfo` |

The two do not imply each other. A pull from Emby is operator-triggered
(`source = manual`) and platform-supplied (`producer = platform:emby`). A provider
refresh also records `manual`, so before `producer` existed a provider write that
overwrote an operator's field was indistinguishable from the operator typing it.
Migration 029 (`internal/database/migrations/029_metadata_changes_producer.sql`)
adds the column and its header gives the full argument for a second column over a
richer `source` vocabulary. The header's notes on what is stamped describe the
first PR only; `history_producer.go` is current.

## Vocabulary

The authoritative list, with the writer behind each token, is the doc block in
`internal/artist/history_producer.go`. In summary:

| Producer | Meaning |
|---|---|
| `""` | The writer did not record it. Makes no claim. |
| `operator` | A person typed or chose the value. |
| `provider:<name>` | A metadata provider supplied it. Bare `provider:` is valid. |
| `provider:identify_*` | An automated identify tier picked a MusicBrainz ID. A tier, not a provider. |
| `platform:<type>` | A connected platform supplied it via a pull. |
| `rule:<id>` | A rule engine pass supplied it; mirrors `source`. |
| `restore` | A stored value was put back. Asserts no authorship. |
| `nfo` | Parsed from an NFO file during a scan. |
| `filesystem` | Observed on disk (image flags, counts). |

Nothing validates a producer on the write path. `Record` in
`internal/artist/history.go` and `recordHistoryTx` in
`internal/artist/lock_restore.go` store what the context carries, so a misspelled
token is stored and shown verbatim. `validHistoryProducer` documents the accepted
shape and is exercised only by its own test. The one untrusted input, the client
claim on a field edit, goes through `sanitizeProducerClaim` in
`internal/api/handlers_field.go`: a positive allow-list (`operator` or
`provider:<known provider>`), anything else recorded as `""` and never rejected.

## The empty-string contract

`""` is the default and it is not `operator`. It means "the writer did not say",
so it can never be a wrong claim. Defaulting to `operator` would turn every
historical row, and every future unstamped path, into a guess that launders an
automated write as a human decision.

Consequences:

- A legacy row (written before its path stamped, or before migration 029) holds
  `""` and renders "Value source not recorded".
- It is never backfilled. Nothing on the row can supply the answer, and an
  inferred value would be indistinguishable from a recorded one.
- It is never hidden on a value row. Absence must not read as clean. The
  `rule_fix` audit pseudo-field carries no value and shows no label at all
  (`historyProducerShown`).
- A row written after 029 with `""` is either an unstamped path or a deliberate
  empty (a provider-modal merge, an off-allow-list client claim).

## Guardrails

- **Census test.** `TestHistoryWriteSitesAreClassified` in
  `internal/api/history_producer_census_test.go` parses the source and fails when
  a function that can write history is absent from `historySiteClasses`. Each
  site is `stamped`, `caller` (the producer comes from the caller's context) or
  `none` (deliberately unstamped, with a reason). It scans only `internal/api`,
  `internal/rule`, `internal/scanner` and `internal/maintenance`, and matches a
  write verb only on a receiver named `artistService`, `historyService`, `svc` or
  `h`. A writer in another package, under another receiver name, or reached
  through a function value or interface is invisible to it; add the directory to
  the test when writing history from a new package.
- **Runtime WARN.** `resolveProducerForWrite` logs `history: row written with no
  producer set` when neither a per-field overlay entry for that field nor a scalar
  producer is on the context (`producerForField`). The log carries `artist_id`, `field` and `source`, never a value.
  An explicit `ContextWithProducer(ctx, "")` is a deliberate empty and does not
  warn.

## Add a write path

1. Decide what supplies the value, and stamp it on the context with
   `artist.ContextWithProducer` (one producer for the whole write) or
   `ContextWithFieldProducers` (per field) before calling the artist service. A
   per-field overlay alone leaves every field it does not name unset and
   warning, so pair it with a scalar fallback (as `refreshProducerContext` does)
   unless a WARN on unnamed fields is intended.
2. Add the function to `historySiteClasses` (key `<dir>/<file>::<func>`) with its
   class and a reason. For a site in the scanned packages the census fails until
   you do, and it also fails on a stale entry. A site classed `stamped` passes
   only when the write's first argument comes from one of `producerStampers`, so
   a new stamping helper must be added there.
3. If no token fits, extend the vocabulary doc block in `history_producer.go`
   (and `validHistoryProducer`, which mirrors it), add the label in
   `web/templates/history_producer.go` and `internal/i18n/locales/en.json`, and
   update the `producer` description in `internal/api/openapi.yaml`. Extend
   `sanitizeProducerClaim` only if a browser may claim the token.
4. Never default to `operator`. If the writer cannot say, stamp `""` explicitly.
5. Do not log the value. Only ids and the field name.

## How history displays it

`web/templates/history_producer.go` turns a producer into words. Three surfaces
share it: the Activity feed, the per-field prior-values popover, and the artist
history tab.

- The source badge says what started the change. A "Value" label beside it says
  what supplied the value.
- The label is hidden only when it would repeat the badge (producer equals
  source, or a revert whose value is a `restore`) and on the synthetic `rule_fix`
  pseudo-field.
- `provider:identify_*` render as match tiers, never "from <provider>".
- A token with no known prefix is shown verbatim. An unknown provider, platform,
  or rule name is shown raw inside its prefix's phrase. Nothing is mapped to a
  guessed label.
- Meaning is carried by text, not color. On the two chip surfaces (Activity,
  history tab) only the not-recorded state is italic; the popover shows plain
  text.

The lock-damage repair does not read `producer`; see
[Lock damage repair](https://github.com/sydlexius/stillwater/blob/main/docs/architecture/lock-damage-repair.md).
