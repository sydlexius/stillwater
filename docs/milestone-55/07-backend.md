# 07 — Cross-cutting backend (chrome, SSE channels)

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md).
>
> No prototype — this issue is server-side only. UI consumers live in issues 01–06.

## Scope clarification

**v2 is a UX flag, not an API version.** The migration plan ([§1](migration-plan.md#1-architecture-do-not-rewrite)) is explicit: keep server-side templ + HTMX, keep the `/api/v1/*` endpoints, do not introduce a SPA, do not fork serializers. Every screen issue (01–06) reuses the existing handlers and endpoints. There is **no `/api/v2/`**.

This issue therefore covers only:

1. The v2 chrome wiring (`STILLWATER_UX` env flag, `/v2/*` route registration, `LayoutV2` wrapper, per-user opt-in cookie).
2. The handful of SSE channels that screens 01–06 formally subscribe to.
3. Documentation of the existing endpoints those screens depend on, so an implementer can verify nothing is being added.

No DB migrations are required. No new tables. No new endpoints under `/api/v1/` beyond the live-tail logs stream introduced for issue 05.

## v2 chrome wiring

### Phase 0 (one PR, foundation)

1. Add `web/templates/v2/` directory with:
   - `layout.templ` — `LayoutV2(title, assets)` that wraps `Layout` but pulls v2 sidebar/bottom-tab components.
   - `sidebar.templ`, `bottom_tabs.templ` — v2 chrome partials.
2. Add the `STILLWATER_UX` env flag and `/v2/*` route registration in `internal/server/router.go`. The flag controls which template the existing handlers render; **handlers are unchanged**.
3. Add the per-user opt-in cookie (`sw_ux=v2`) read by middleware. Cookie overrides env default.

### `STILLWATER_UX` semantics

| Value | `/` | `/v2/*` |
|---|---|---|
| `v1` (default) | v1 templates | 404 |
| `v2` | redirect to `/v2/` | v2 templates |
| `dual` (default during the milestone) | v1 templates *or* chooser if no cookie | v2 templates |

- Per-request response header `X-Stillwater-UX: v1|v2` so QA can verify which build served a request without inspecting cookies.
- Per-request log line: `ux=v1|v2 user=<id> path=<…>` so we can see uptake during the beta window.

### CSRF + base-path injection — preserve

The global `htmx:configRequest` listener in `layout.templ` prefixes paths and adds `X-CSRF-Token`. v2 markup must use absolute paths (`/api/v1/...`) so this hook applies. **Do not bypass it.**

### Asset pipeline — preserve

`AssetPaths` already carries cache-busted URLs for every JS bundle. v2 reuses it; do not fork.

## SSE event bus

A single endpoint:

```
GET /api/v1/events            (Content-Type: text/event-stream)
```

This endpoint exists in v1; v1 templates already subscribe to a subset of the events below. v2 formalises the catalog and adds `logs.line` / `logs.throttled` for the new logs viewer (issue 05).

Event names use dotted scopes: `<scope>.<thing>.<verb>`. Payloads are always JSON.

| Event | Payload | Emitted by | New in v2? |
|---|---|---|---|
| `activity.scan.started` | `{ libraryId, ts }` | scanner | no |
| `activity.scan.finished` | `{ libraryId, ts, artistsScanned, durationMs }` | scanner | no |
| `activity.recent` | `{ ts, kind, text, artistId? }` | various; aggregated for the dashboard rail | **yes** (channel exists, dashboard subscribes for first time) |
| `violations.recomputed` | `{ ts, opened, closed }` | rules engine | no |
| `settings.changed` | `{ sectionId, updatedBy, ts }` | settings PATCH handler | **yes** (channel new) |
| `report.run.progress` | `{ reportName, percent, etaSec }` | reports runner | no (now formally documented) |
| `bulk.progress` | `{ jobId, completed, total, failed, etaSec }` | bulk runner | no (now formally documented) |
| `bulk.finished` | `{ jobId, status, completed, total, failed }` | bulk runner | no (now formally documented) |
| `artist.refresh.progress` | `{ artistId, provider, status, fieldsUpdated? }` | refresh runner | no (now formally documented) |
| `dashboard.action-resolved` | `{ violationId }` | undo timer expiry | no (existing body event, also re-emitted on SSE for cross-tab badge update) |
| `logs.line` | `{ ts, level, scope, message, meta }` | log multiplexer | **yes** |
| `logs.throttled` | `{ suppressedCount, windowMs }` | log multiplexer | **yes** |

### Client behaviour

- Auto-reconnect with exponential backoff capped at 30 s.
- `Last-Event-ID` header carries the last seen event id; server replays from in-memory ring buffer (capped at 1,000 events / 5 minutes — whichever comes first).
- On reconnect after the buffer was lost, the client refetches affected derived state (dashboard summary, palette index) instead of trying to replay.

## Endpoint catalog (existing — for reference)

Every screen issue calls only `/api/v1/*` endpoints. This list is for verification — none of these are new in v1.6.0 unless marked.

### Dashboard (issue 01)

```
GET  /dashboard
GET  /dashboard/actions?q=&severity=&category=&cursor=
POST /api/v1/notifications/{id}/fix
POST /api/v1/fix-undo/{id}
POST /api/v1/rules/run
GET  /api/v1/health/stats
```

### Artists list (issue 02)

```
GET  /artists
GET  /artists/list                               (HTMX fragment, tri-state filter encoding)
POST /api/v1/artists/bulk/run-rules
POST /api/v1/artists/bulk/re-identify
POST /api/v1/artists/bulk/fetch-images
GET  /api/v1/compliance                          (feeds ComplianceMap)
```

### Artist detail (issue 03)

```
GET   /artists/{id}
GET   /artists/{id}/tab/{tab}                    (HTMX tab swap)
PATCH /api/v1/artists/{id}/fields/{name}
POST  /api/v1/artists/{id}/fields/{name}/lock
POST  /api/v1/artists/{id}/lock
GET   /api/v1/artists/{id}/fields/{name}/providers
POST  /api/v1/artists/{id}/refresh
POST  /api/v1/artists/{id}/run-rules
POST  /api/v1/artists/{id}/identify
POST  /api/v1/artists/{id}/rename
GET   /api/v1/artists/{id}/platform-state/{conn}
```

### Reports (issue 04)

```
GET  /reports
GET  /reports/{name}
POST /api/v1/reports/{name}/run
GET  /api/v1/reports/{name}/export.csv
GET  /api/v1/compliance/matrix?groupBy=&hidePassing=
```

### Logs (issue 05) — **one new endpoint**

```
GET /api/v1/logs?level=&scope=&q=&cursor=&limit=
GET /api/v1/logs/download
GET /api/v1/logs/stream?level=&scope=&q=         ← new in v1.6.0 (SSE)
```

### Settings (issue 06)

```
GET   /settings
GET   /settings/{sectionId}
PATCH /api/v1/settings/{sectionId}
POST  /api/v1/settings/{sectionId}/reset
GET   /api/v1/settings/{sectionId}/export
```

### Per-user preferences (issue 06, new section)

A per-user preferences endpoint already exists (driven by the `swPreferences` JS module on the client). v1.6.0 adds two new keys; the endpoint contract is unchanged.

```
GET   /api/v1/preferences
PATCH /api/v1/preferences         (partial; merge semantics)
```

New keys for v1.6.0:

| Key | Type | Default | Used by |
|---|---|---|---|
| `artist_detail_section_order` | comma-separated string | `"identifiers,bio,artwork,providers,findings,activity,compliance"` | Issue 03 — artist detail section render order |
| `artist_detail_hidden_sections` | comma-separated string | `""` | Issue 03 — sections the user hides |

`identifiers` is **never** moved or hidden by the UI — the renderer always pins it to the top regardless of these keys' values. (The keys are still permitted to contain `identifiers`; the renderer ignores it. This keeps the storage shape simple.)

## Finding string contract

Findings surface on multiple screens (Reports rows, artist detail field chips, dashboard action queue, embedded findings section). All four surfaces consume the same shape; do not let surface-specific rendering creep into the producer.

```ts
type Finding = {
  rule:        string;                // "area.detail" dot-notation, e.g. "bio.length", "image.aspect"
  severity:    "err" | "warn" | "info";
  title:       string;                // ≤ 60 chars, sentence-case, says *what's wrong*
  message:     string;                // full sentence with concrete values
  suggestedFix: string;               // imperative ("Rewrite…", "Promote…", "Run…")
  evidence?:   string;                // optional: file path, or "logs:component:level"
};
```

Field-by-field rules:

| Field | Required | Rendered as | Constraint |
|---|---|---|---|
| `title` | yes | Chip label, list-row title, sticky-header tooltip | Stands alone without `message`; truncation is unacceptable on chips |
| `message` | yes | Drawer body, popover detail | Names concrete values (which provider, which threshold, what was found) |
| `suggestedFix` | yes | Drawer "Fix it" hint | Imperative voice, one sentence, no questions |
| `rule` | yes | Tooltip + link to rule docs only | Never rendered as primary text |
| `evidence` | no | Drawer "View source" link | When prefixed `logs:`, surfaces the error-deep-link to Logs (see below) |

### i18n keys

Every rule provides translations under a fixed key path:

```
findings.{rule}.title
findings.{rule}.message      (template; takes named slots)
findings.{rule}.fix
```

E.g. for `bio.length`:

```
findings.bio.length.title    = "Biography too short"
findings.bio.length.message  = "Biography is {{.Have}} chars; rule expects ≥ {{.Want}}."
findings.bio.length.fix      = "Re-fetch from Wikidata or write manually."
```

The rule engine emits the rule code + slots; the templ layer resolves the strings via the existing `t(ctx, key, slots)` helper. Strings are **not** baked into Go.

This applies to every existing rule plus any added in v1.6.0. Migrating the seven hard-coded rule strings currently in `rules.go` to translation keys is part of issue 07's acceptance.

## Error-only Logs deep-links

Three surfaces deep-link to Logs with filters pre-applied. **All are URL-only — no API change.**

| From | When | URL produced |
|---|---|---|
| Finding row (severity `err`) | Always | `/v2/logs?component={evidenceComponent}&level=error&artist_id={artistId}&since={ago.1h}` |
| Failed-action toast | Toast text contains the failure | `/v2/logs?component={inferredComponent}&level=error&artist_id={artistId}&since={ago.1h}` |
| Reports error chip | Severity is `err` | `/v2/logs?rule={rule}&level=error&since={ago.1h}` |

Logs (issue 05) must already accept these query-string parameters as initial filter state and disable auto-follow until the user clicks `Jump to bottom` (matching the existing `?since=...` behaviour).

Deep-links are **not** generated for warn/info findings, activity rows ("Re-fetched MusicBrainz"), or any non-error context. The link is for debugging breakage, not browsing.

## Database changes

**None.** No new tables, no column additions, no migrations. The screens reuse existing v1 storage:

- `settings` table already records `updated_by_user_id` and `updated_at` (relied on by issue 06).
- Reports built-ins live in code, not DB; runs are log-line events (issue 04 last-run-pill reads from logs, not a `report_runs` table).
- Bulk-job progress is held in-memory by the existing runner; it's not durable across restarts. v1.6.0 does not change that. (A durable bulk-job table is on a future roadmap but is **not** part of this milestone.)
- Logs are file-backed (rotated on disk); the live-tail stream reads from the same source.

If a future milestone needs durable saved reports, scheduled reports, persistent bulk-job state, or an onboarding state table, those each warrant their own milestone with explicit migration plans.

## Performance budgets

The screens promise these numbers; the backend has to make them real. None of these are new endpoints (except the logs stream); the budgets are for v2's added load.

| Endpoint | Budget (warm cache, 5k-artist DB) |
|---|---|
| `GET /dashboard` (HTMX render) | ≤ 100 ms p95 |
| `GET /artists/list` (HTMX, page of 50) | ≤ 150 ms p95 |
| `GET /api/v1/compliance/matrix` (5k × 22) | ≤ 800 ms p95, ≤ 250 KB gzipped |
| `GET /api/v1/logs/stream` first-byte | ≤ 200 ms |
| SSE event delivery (server-side dispatch → client) | ≤ 250 ms p95 |

## Auth + roles

No role changes in v1.6.0. The existing `admin` / `viewer` split is preserved across all v2 templates:

- `viewer` may: GET everything, run reports, export CSV, dispatch the dashboard `Fix` flow on resolvable violations.
- `admin` only: PATCH settings, POST bulk-jobs, configure connections, run rules globally, edit artist fields.

v2 templates honour role-conditional rendering exactly as v1 does.

## Acceptance

### Chrome
- [ ] `web/templates/v2/layout.templ` `LayoutV2(title, assets)` exists; pulls v2 sidebar/bottom-tabs.
- [ ] `STILLWATER_UX=dual` runs both UIs against the same DB without errors in a clean container.
- [ ] `STILLWATER_UX=v2` boot serves `/v2/dashboard` as the index for an authenticated session.
- [ ] `sw_ux` cookie overrides the env default.
- [ ] `X-Stillwater-UX` response header is set on every templ-rendered response.
- [ ] CSRF + base-path injection still apply to v2 markup.

### SSE
- [ ] Every event in the table fires from the right code path; integration test asserts each.
- [ ] `Last-Event-ID` replay works from the in-memory buffer.
- [ ] Reconnect backoff is bounded; verified with a forced disconnect test.
- [ ] `logs.line` / `logs.throttled` honour server-side filters from the `?level=&scope=&q=` query string.

### API
- [ ] Smoke test for each screen issue confirms v2 templates call only the endpoints catalogued above (no `/api/v2/*` calls anywhere — `grep -r 'api/v2' web/templates/v2/` is empty).
- [ ] `GET /api/v1/logs/stream` is the only **new** endpoint in v1.6.0.
- [ ] No DB migration runs in CI for v1.6.0.

### Performance
- [ ] Each row in the budget table has a benchmark in CI; build fails on regression > 30%.

### Findings + i18n
- [ ] All seven existing hard-coded rule strings in `rules.go` migrated to translation keys under `findings.{rule}.{title|message|fix}`.
- [ ] Rule engine emits the rule code + slot values; resolution happens in the templ layer.
- [ ] `Finding.title` constraint (≤ 60 chars, stands alone) enforced in lint test over the translation bundle.
- [ ] `evidence` field round-trips through the JSON serializer used for the action queue and reports rows (regression test).

### Error-only Logs deep-links
- [ ] Issue 05's filter parser accepts `component`, `level`, `artist_id`, `rule`, `since` from URL query string and pre-populates the filter bar.
- [ ] Findings rows with severity `err` render a `View logs` link with the URL shape above; warn/info do not.
- [ ] Failed-action toast helper accepts an optional `logs` argument and renders the link when present.

### Per-user preferences
- [ ] New keys `artist_detail_section_order` and `artist_detail_hidden_sections` accepted by `PATCH /api/v1/preferences` with merge semantics.
- [ ] Renderer pins `identifiers` to top regardless of stored value (lint test asserts this with a fixture that places `identifiers` last in the order key).

### Docs
- [ ] `docs/api.md` — confirms the `/api/v1/*` surface is unchanged except for the logs stream.
- [ ] `docs/sse.md` (new) — event catalog, reconnect semantics, the table above.
- [ ] `docs/findings.md` (new) — finding shape, i18n key convention, rule-engine contract.
- [ ] `docs/configuration.md` — `STILLWATER_UX` documented.
- [ ] `CHANGELOG.md` line.
