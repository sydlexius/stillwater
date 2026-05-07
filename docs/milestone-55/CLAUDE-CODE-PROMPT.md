# Claude Code handoff — Stillwater Milestone 55 (v1.6.0 UX Refresh)

> Drop this prompt into a fresh Claude Code session in the `stillwater` repo. It tells the agent everything it needs to start, and where to look for everything it doesn't.

---

## Your job

Implement Milestone 55 of `sydlexius/stillwater`: a UX refresh that ships under a feature flag (`STILLWATER_UX=v2`) without breaking v1. The milestone is broken into seven issues, all under `docs/milestone-55/`.

**Read these in order before writing any code:**

1. `docs/milestone-55.md` — milestone overview, goals, dual-route flag mechanics, global keyboard shortcuts.
2. `docs/milestone-55/migration-plan.md` — **the v1 source-of-truth.** Every issue defers to this doc. Do not start an issue without reading the relevant section here. Section numbering matches the issue numbers.
3. `docs/milestone-55/07-backend.md` — cross-cutting backend (chrome, SSE catalog, finding contract, error-only Logs deep-link contract, per-user preferences). Every screen issue references this; read it once, refer back as needed.
4. The screen issue you're picking up: `01-dashboard.md` … `06-settings.md`.

**Working prototypes live in two places** and are the visual source-of-truth:

- `screens/*.html` — full hi-fi prototypes, JSX-backed, drive against mocked data. Drive these in a browser to see the intended interaction model.
- `docs/milestone-55/prototypes/standalone/*.html` — self-contained single-file bundles (offline) for issue attachments. Same designs as `screens/`, no JSX dependency.

A given issue may reference one, the other, or both. Where they disagree, **the spec doc wins**, then `screens/*.html`, then the standalone bundle.

---

## Hard rules (these have been re-litigated; do not relitigate)

1. **No new API surface.** Every screen reuses `/api/v1/*` endpoints. The **only** new endpoint in the whole milestone is `GET /api/v1/logs/stream` (SSE) for issue 05. No `/api/v2/`. No new REST routes for any other screen.
2. **No DB migrations.** Zero. No new tables, no column additions. Several earlier prototypes proposed saved reports, scheduled reports, durable bulk-job state, and onboarding state tables — all are explicitly **out of scope** for v1.6.0 and called out in the migration plan.
3. **Do not fork existing components.** `FieldDisplay`, `FilterFlyout` (the tri-state one), `BulkActionBar`, `BulkProgressPill`, `Pagination`, `ColumnToggle`, `StatusBadge`, `ImageStatusBadge`, `<SettingRow>`, `UndoToast`, `showUndoToast` are reused byte-for-byte under v2. v2 wraps them in new layouts; v2 does not reimplement them. The migration plan calls out specific regressions earlier prototypes introduced — read it.
4. **Preserve the `id`s and class names that v1 JS hooks against.** Examples: `#refresh-panel`, `#bulk-progress-pill`, `data-clear-ids`, `data-density`, the platform-badge class names. The migration plan lists every one.
5. **Locked-field rendering is enforced visually in v1** (`FieldDisplay` removes pencil/clear/fetch from the DOM, not just disables them). v2 keeps this, plus a defensive 409 server-side.
6. **Compliance-dot suppression on `nil` map.** Do not render a grey placeholder. Hide the dot.
7. **Bulk actions only enabled when a filter narrows the set.** This is a deliberate safety rail; do not remove it.
8. **Off-page selection (`?ids=…`) and `data-clear-ids` opt-out** must continue to work across pagination. Issue #1227 in the v1 codebase is the canonical reference.
9. **CSRF + base-path injection** via the `htmx:configRequest` listener applies to v2. v2 markup must use absolute paths; do not bypass it.
10. **No SPA. No client-side router.** v2 is server-rendered templ + HTMX, same as v1.

---

## Architecture in one paragraph

`STILLWATER_UX` env flag (`v1` | `v2` | `dual`) and a `sw_ux=v2` per-user cookie pick which template tree renders. Handlers are unchanged; only the template they reach for changes. v2 templates live under `web/templates/v2/` and import the same data structs the v1 templates use. The `LayoutV2(title, assets)` shell wraps the existing `Layout` and pulls v2 sidebar/bottom-tab partials. Cache-busted asset paths come from the existing `AssetPaths`. CSRF + base-path injection is the existing global `htmx:configRequest` hook. SSE is one endpoint (`GET /api/v1/events`) with a new dotted-name catalog (see 07-backend.md table). For Logs (issue 05) only, a new SSE endpoint `GET /api/v1/logs/stream` carries server-side-filtered live tail.

---

## Recommended working order

1. **07-backend, Phase 0** (chrome scaffold). One PR. Lands `web/templates/v2/{layout,sidebar,bottom_tabs}.templ`, the `STILLWATER_UX` flag, the `/v2/*` route registration, the `sw_ux` cookie, the `X-Stillwater-UX` response header, and the per-request UX log line. **No screen content yet.** Acceptance: `STILLWATER_UX=dual` boots, both `/dashboard` (v1) and `/v2/dashboard` (404 stub) respond.
2. **07-backend, SSE catalog formalisation.** One PR. Documents existing channels, adds `activity.recent`, `settings.changed`. Lands the `Last-Event-ID` replay buffer if not already present. Does not yet add `logs.line` / `logs.throttled` (those land with issue 05).
3. **07-backend, finding string contract + i18n migration.** One PR. Migrates the seven hard-coded rule strings in `rules.go` to translation keys under `findings.{rule}.{title|message|fix}`. Adds the lint test that enforces `Finding.title ≤ 60 chars`. Adds `docs/findings.md`. **Required before issue 03 can ship its field-level finding chips correctly.**
4. **07-backend, preferences keys.** One PR. Adds `artist_detail_section_order` and `artist_detail_hidden_sections` to the preferences schema with merge semantics on PATCH. Adds the renderer-side helper that pins `identifiers` to top regardless of stored value. **Required before issue 03's section-reorder support and issue 06's preferences card.**
5. **01-dashboard.** Two PRs is reasonable: (a) the v2 templates + restyled chrome + compact header, (b) the right-rail `activity.recent` SSE consumer. Keep the existing endpoints, the undo flow, and the filter-preserving reload contract.
6. **02-artists.** One PR. v2 chrome around the existing components. Verify the existing `artists_offpage_selection_test.go` and `artists_pagination_listener_test.go` continue to pass against v2 templates.
7. **03-artist-detail.** **The heaviest issue.** Earlier prototypes got it wrong in specific ways called out in `migration-plan.md §2.1` — **read it carefully**. The tab set is fixed (Overview · Images · Providers · Discography · History · Violations · Debug). `FieldDisplay` is not forked. Aliases and Members are not dropped. The slideshow header stays. The `#refresh-panel` SSE pattern stays. New surfaces this milestone: sticky mini-header, field-level finding chips, lightbox Crop & Trim, render-side wiring for the per-user section-order preference. Land in this order: (a) tab restyle + slideshow tightening + sticky mini-header, (b) field-level finding chips + error-only Logs deep-link, (c) lightbox Crop & Trim, (d) section-order renderer. The Settings card that edits the preference (issue 06) ships separately and is not blocking.
8. **04-reports.** One PR. Two-pane workspace with the matrix tab. Saved/scheduled reports are out of scope.
9. **05-logs.** Two PRs: (a) `GET /api/v1/logs/stream` SSE endpoint + filter parser + rate-limit + `logs.line` / `logs.throttled` events, (b) the v2 page consuming it, including the inbound deep-link contract (`?component=&level=&artist_id=&rule=&since=`).
10. **06-settings.** One PR. The four-group rail + filter index + the new "Per-user preferences" section's "Artist detail layout" card.

---

## Per-issue verification gates

Each issue has its own `## Acceptance` block. **Do not mark an issue done without ticking every box.** The boxes have been written specifically to catch the regressions earlier prototypes introduced.

In addition, every PR must:

- Include a `CHANGELOG.md` line under `## [1.6.0]`.
- Include or update the relevant `docs/<screen>.md` file.
- Run the existing tests; the migration plan lists which v1 tests must continue to pass for each issue.
- For SSE work, include a Cypress test that asserts the event lands in the right surface within 1 s of dispatch.

---

## Where to look first when stuck

| Situation | Read |
|---|---|
| "What did v1 do here?" | `migration-plan.md` (matches issue number) → then the specific `web/templates/*.templ` it cites |
| "What's the URL shape for filters?" | The existing v1 templ source. Do not invent a new encoding. |
| "Should I add a new endpoint?" | No. Re-read 07-backend.md's endpoint catalog. The only new endpoint is the logs stream. |
| "Should this be a tab in artist detail?" | The tab set is locked: Overview · Images · Providers · Discography · History · Violations · Debug. Anything else does not get a tab. |
| "Which finding string goes where?" | 07-backend.md § Finding string contract. `title` on chips/rows, `message` in popovers, `suggestedFix` in drawers, `rule` in tooltips only. |
| "Should this surface deep-link to Logs?" | Only if it's reporting an error. 07-backend.md § Error-only Logs deep-links. |
| "Where does this preference live?" | Per-user keys go on `/api/v1/preferences` (existing endpoint, merge semantics). Org-level settings go on `/api/v1/settings/{sectionId}`. The two are distinct. |

---

## Anti-patterns (if you find yourself doing one of these, stop and re-read the migration plan)

- Adding a column to an existing table.
- Inventing a new tab in artist detail beyond the seven listed.
- Forking `FieldDisplay` to "simplify" it.
- Replacing the tri-state filter flyout with a flat multi-select.
- Adding `/api/v2/anything`.
- Replacing `Pagination` with virtualized infinite scroll.
- Collapsing the refresh-panel into a toast.
- Rendering the compliance dot as grey when the map is `nil`.
- Adding a `findings.go` constant for finding strings instead of putting them in the translation bundle.
- Generating Logs deep-links from non-error surfaces.
- Letting `Identifiers` move or be hidden in the artist-detail layout preference.

---

## When you've finished a PR

Open it against `main`, link to the issue file path (`docs/milestone-55/0X-*.md`), and in the description quote the specific acceptance boxes you've ticked. Reference the `migration-plan.md` section if the PR preserves a v1 contract that an earlier prototype broke.

**Do not** open the next issue's PR until the current one is merged. The issues have shared dependencies (07-backend's preferences keys block 03 and 06; finding contract blocks 03; logs SSE blocks the consumer in 05). Out-of-order shipping creates merge conflicts and stale specs.
