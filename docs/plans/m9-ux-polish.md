# M9: UX Polish (v0.9.0)

## Goal

First-run onboarding wizard, mobile-responsive design, and logo/favicon. Make the application approachable for new users and usable on all devices.

## Prerequisites

- M8 (Security and Stability) complete -- backup and credential reset provide safety net for onboarding

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 59 | OOBE multi-step onboarding wizard | plan | opus |
| 29 | Mobile-responsive design pass | direct | sonnet |
| 30 | Logo and favicon design (SVG, theme-adaptive) | direct | sonnet |

## Implementation Order

### Step 1: Logo and Favicon (#30)

Quick win that improves the look of everything else (mobile nav, onboarding wizard).

1. Design SVG logomark:
   - Abstract flowing water / sound wave motif
   - Simple enough to work as 16x16 favicon
   - Use CSS `currentColor` for theme adaptivity (dark on light, light on dark)

2. Create favicon set:
   - `web/static/favicon.ico` (16x16 + 32x32 multi-size)
   - `web/static/favicon-32x32.png`
   - `web/static/favicon-16x16.png`
   - `web/static/apple-touch-icon.png` (180x180)
   - `web/static/site.webmanifest`

3. Integrate into layout:
   - Add logo SVG to `web/templates/layout.templ` navbar (replace text-only "Stillwater")
   - Add favicon links in HTML `<head>`
   - Test dark/light variant switching

### Step 2: Mobile-Responsive Design (#29)

1. Audit all templates at mobile breakpoints (320px, 375px, 428px):
   - Dashboard (index.templ)
   - Artist list (artists.templ)
   - Artist detail (artist_detail.templ)
   - Settings (settings.templ)
   - Reports pages (compliance, dashboard, nfo_diff)
   - Login/setup pages

2. Fix layout issues:
   - Tables: add horizontal scroll wrapper (`overflow-x-auto`, already on artist table)
   - Grids: single-column on mobile (`grid-cols-1` default, `sm:grid-cols-2`, `lg:grid-cols-4`)
   - Cards: full-width on mobile, side-by-side on desktop
   - Forms: stack labels above inputs on mobile

3. Add mobile navbar:
   - Hamburger menu button visible below `md` breakpoint
   - Slide-out or dropdown nav panel
   - Close on outside click, escape key, or navigation
   - Active page indicator

4. Touch targets:
   - Minimum 44px height on all interactive elements
   - Adequate spacing between tap targets
   - Test image crop modal on mobile viewport

### Step 3: OOBE Onboarding Wizard (#59)

Multi-step wizard that launches after first admin login. Implement in phases.

#### Phase 1: Wizard Skeleton

1. Create `web/templates/wizard.templ`:
   - Step indicator (numbered circles with connecting lines)
   - Previous/Next/Skip navigation buttons
   - Content area for each step
   - Progress persisted via `settings` table (`oobe_completed`, `oobe_step`)

2. Add wizard state management:
   - `GET /api/v1/settings/oobe` -- returns current wizard state
   - `PUT /api/v1/settings/oobe` -- update wizard progress
   - `handleIndex` redirects to `/setup/wizard` if `oobe_completed` is false and user is authenticated

3. Add route: `GET /setup/wizard` -- renders wizard page

#### Phase 2: Welcome Step

- Brief project description
- What Stillwater manages (NFO files, images, metadata)
- Links to documentation and GitHub

#### Phase 3: Music Library Step

- Input field for music library path (pre-filled with `SW_MUSIC_PATH` or `/music`)
- Validation: check path exists and is readable
- Show directory listing preview (first 10 entries)
- Save to settings on "Next"

#### Phase 4: Platform Profile Step

- Radio buttons: Emby, Jellyfin, Kodi, Custom
- Brief description of what each selection configures
- Sets NFO format preferences and image naming defaults
- Save to `settings` table

#### Phase 5: Provider API Keys Step

- Card per provider (Fanart.tv, TheAudioDB, Discogs, Last.fm)
- Input field for API key with "Get key" link
- "Test" button that validates the key via provider API
- Status indicator (untested, valid, invalid)
- TheAudioDB shows as "optional" since it works with free key (M8 fix)
- Save each key individually on blur or test

#### Phase 6: External Connections Step

- Form to add Emby/Jellyfin/Lidarr connections
- Reuse connection create/test logic from M7
- "Test Connection" button with status feedback
- List already-added connections with status

#### Phase 7: Scanner Configuration Step

- Show current exclusion list with ability to add/remove
- Scan depth setting (if applicable)
- "Start First Scan" button with progress indicator
- Uses existing scan button polling pattern from artists.templ

#### Phase 8: Review and Finish Step

- Recap status of each step (completed, skipped, incomplete)
- Option to go back to any incomplete step
- "Finish Setup" button marks `oobe_completed = true`
- Redirects to dashboard

#### Phase 9: Settings Integration

- Add "Run Setup Wizard" button in settings page
- Resets `oobe_completed` to false and redirects to wizard
- Useful for reconfiguration or showing new users

## Key Design Decisions

- **Logo before mobile:** The logo is needed for the mobile hamburger nav and the wizard welcome step.
- **Mobile before wizard:** The wizard must be usable on mobile since self-hosted users may configure from a phone.
- **Wizard phases are incremental:** Each phase can be a separate commit/PR. State is persisted between steps so partial completion survives page reloads.
- **Wizard reuses existing services:** Does not duplicate connection/webhook/settings logic from M7. Calls the same API endpoints.
- **Config applied per-step:** Each wizard step saves immediately rather than batching at the end. This prevents data loss if the user abandons the wizard mid-way.

## Verification

- [ ] Logo renders correctly in dark and light themes
- [ ] Favicon appears in browser tab
- [ ] All pages responsive at 320px, 375px, 428px viewports
- [ ] Hamburger menu works on mobile
- [ ] Touch targets meet 44px minimum
- [ ] Wizard launches on first admin login
- [ ] Each wizard step saves config immediately
- [ ] Wizard can be skipped and resumed
- [ ] "Run Setup Wizard" button works in settings
- [ ] Wizard is usable on mobile viewports
- [ ] `go test ./...` and `golangci-lint run` pass
- [ ] Tag v0.9.0
