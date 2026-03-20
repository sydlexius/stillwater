# MusicBrainz Configure Panel

## Goal

Replace the collapsed "Mirror Settings" section on the MusicBrainz provider card
with a discoverable "Configure" button and unified configuration panel that supports
server selection (official, beta, custom mirror) with future-proofed OAuth credential
placeholders.

## Components

### 1. Configure Button

Add a "Configure" button to the action-button area of the provider card for any
provider with `SupportsBaseURL == true`. This button appears in the same position
as the Configure/Update button on keyed providers (far right of the card header).

Condition: `pk.SupportsBaseURL` (independent of `RequiresKey`/`OptionalKey`).

### 2. Configuration Panel

An inline section (same pattern as the API key input panel) toggled by the Configure
button. Contains three subsections:

#### Server Selection

Radio group with three options:

| Option   | Base URL                              | Rate Limit | Notes              |
|----------|---------------------------------------|------------|--------------------|
| Official | `https://musicbrainz.org/ws/2`        | 1 req/s    | Default selection   |
| Beta     | `https://beta.musicbrainz.org/ws/2`   | 1 req/s    | Same production data|
| Custom   | User-provided                         | User-set   | Reveals extra fields|

When "Custom" is selected, two additional fields appear:
- **Base URL** -- `<input type="url">`, required, placeholder `http://192.168.1.100:5000/ws/2`
- **Rate Limit** -- `<input type="number">`, min=1, max=100, default=10, suffix "req/s"

#### OAuth Credentials (Placeholder)

Two disabled fields with a "coming soon" note:
- **Client ID** -- `<input type="text" disabled>`
- **Client Secret** -- `<input type="text" disabled>`
- Helper text: "Required for submitting edits to MusicBrainz (coming soon)"

These fields are non-functional in this iteration. They exist to communicate planned
capability and to reserve the UI layout so adding OAuth later does not require a
redesign.

#### Action Buttons

- **Save** -- persists the selected server config
  - Official: `DELETE /api/v1/providers/{name}/mirror` (revert to default)
  - Beta: `PUT /api/v1/providers/{name}/mirror` with beta URL and 1 req/s
  - Custom: `PUT /api/v1/providers/{name}/mirror` with user-provided URL and rate
- **Test** -- `POST /api/v1/providers/{name}/test` (existing endpoint)
- **Cancel** -- collapses the panel

If a mirror is currently active (not official), show a **Clear** button that calls
DELETE and reverts to official.

### 3. Status Display

The provider card header shows the active server:
- When official: no extra indicator (current behavior)
- When beta or custom: show a badge (like the current "Active" badge) with the
  server name or "Mirror" label

## Backend Changes

None required. The existing endpoints handle all cases:
- `PUT /api/v1/providers/{name}/mirror` -- set base URL + rate limit
- `DELETE /api/v1/providers/{name}/mirror` -- revert to default
- `POST /api/v1/providers/{name}/test` -- test connection

"Official" is represented by the absence of a mirror config (DELETE clears it).
"Beta" is a mirror config with the beta URL. "Custom" is a mirror config with a
user-provided URL.

## Template Changes

### Files Modified

- `web/templates/settings.templ` -- `ProviderKeyCard` component:
  - Add Configure button in the action buttons area
  - Replace the `pk.SupportsBaseURL` mirror section (lines 2617-2709) with the
    new configuration panel
  - Add JS for radio group show/hide of custom fields

### Files NOT Modified

- No backend Go files
- No migration files
- No route changes
- No OpenAPI spec changes (endpoints unchanged)

## HTMX Integration

Save uses the same `hx-put`/`hx-delete` targeting `#provider-card-{name}` with
`innerHTML` swap, which re-renders the entire card with updated state. This is the
existing pattern.

## Accessibility

- Radio group uses `<fieldset>` + `<legend>` for screen readers
- Custom fields use `aria-describedby` linking to the helper text
- Disabled OAuth fields use `aria-disabled="true"` with explanatory text
- Configure button includes `aria-expanded` state tracking
