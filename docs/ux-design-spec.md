# Stillwater UX Design System - Portable Spec

Source-grounded snapshot of the "next/" channel design system, captured for
(a) porting to a new project (Canticle) and (b) seeding an automated UX /
prefs conformance gate. Every token below is either read directly from
source or cross-checked against the #1894 rendered pre-flight capture
(`/tmp/m55-close/uxpreflight/1894-preflight.md`, live page at branch HEAD,
dark theme). Where no value could be confirmed, it is marked `TODO: confirm`.

Primary sources:
- `web/static/css/design-tokens.css` - canonical `:root`/`.dark` token layer
- `web/static/css/input.css` - utilities + component CSS (7480 lines; `.sw-dash-card`, `.sw-toggle-*`, checkbox restyle, etc.)
- `web/components/*.templ` - reusable badge/toggle/modal components
- `web/templates/next/`-equivalent canonical templates (`artists_table.templ`, `dashboard.templ`, `shared_artist_detail.templ`)
- `web/templates/artist_duplicates.templ` - merge modal (contains the checkbox anti-pattern, see §5)
- `internal/i18n/locales/en.json` - copy conventions
- `web/templates/sidebar.templ` / `sidebar_helpers.go`, `web/static/js/sidebar.js` - sidebar structure + collapse behavior
- `web/static/js/keyboard.js` - the full keyboard-shortcut registry/dispatch model (§10)
- `web/components/cheat_sheet_modal.templ`, `context_menu.templ`, `filter_flyout.templ`, `command_palette.templ`, `error_toast.templ`, `skeleton.templ` - menus, flyouts, toasts, loading states
- `web/templates/layout.templ` - the global toast manager (inline script, ~line 347) and `htmx:responseError` handling

> **Status of this capture.** Snapshot taken during the Milestone 55 (v1.6.0)
> UX close-out. It documents the sanctioned design-system *intent*; where the
> codebase currently deviates, that debt is called out inline as "do not port".
> Two debts flagged below are being remediated under issue #1894 as this doc
> lands: the merge-modal checkbox treatment (§5 - resolved with the neutral,
> non-blue pattern described there) and off-scale raw-px type on the
> artist-detail and reports surfaces (§2 - converted to the pref-aware rem
> scale). Sections note the resolution where relevant.

---

## 1. Principles

1. **Dark-default.** `.dark` is "the primary/default theme" (design-tokens.css:169 comment). Light theme is a full override block, not the baseline.
2. **Restraint - no primary/CTA button.** Confirmed on the rendered artist-detail and dashboard pages: every top-bar action button (Edit / Actions / Refresh / Run Rules) shares one transparent, ghost-bordered style. There is no blue "primary action" button anywhere outside of two exceptions: the sidebar active-link and destructive-confirm buttons inside modals (see §3). Rule: **do not add a filled/blue primary button to a toolbar.** Primary/secondary contrast, where needed at all, is resolved only inside modals (Cancel vs. Confirm).
3. **Frosted-glass surfaces.** Cards are a flat semi-transparent tint (`rgba` background), not a `backdrop-filter: blur()` in the canonical `.sw-dash-card` motif (that property is explicitly `none` there - see §4). A separate `.sw-glass` / `.sw-card` utility class *does* use real `backdrop-filter: blur(18px) saturate(180%)` for other chrome (sidebar, bottom bars). Pick per surface: dashboard/detail content cards = flat tint; chrome (sidebar, bottom sheet, filter flyout) = true blur.
4. **Minimal-JS.** HTMX + vendored Cropper.js/Chart.js only; no framework. Interactive disclosure prefers native `<details>/<summary>` over hand-rolled JS accordions (see §5).
5. **Firefox first-class.** Explicit design constraint (repo-wide instruction); avoid Chromium-only CSS features without a Firefox-tested fallback.
6. **Accessibility is load-bearing, not decorative.** Every non-text status indicator (badges, sync/lock/severity pills) carries `role="img"` + `aria-label`, not color alone. Contrast fixes are tracked and cited (e.g. health-score colors bumped from Tailwind 600→700 shades in light mode specifically for axe color-contrast, see `web/components/badge.templ:5-7`).
7. **Rendered evidence only, for any visual claim.** See §7 - this is a process principle as much as a design one, and it is how the merge-modal checkbox bug was actually caught (static reading of the template alone would have missed the disabled-state inversion).

---

## 2. Typography

### Font families (all swappable via one token)

| Role | Token | Value | Notes |
|---|---|---|---|
| Body/UI (canonical) | `--sw-font-family` → `--sw-font-sans` | `'Inter', -apple-system, BlinkMacSystemFont, sans-serif` (default) | User-pickable via `[data-font-family]`: `system`, `inter`, `atkinson` (Atkinson Hyperlegible, an accessibility-oriented face). Confirmed rendered on artist-detail: `"Atkinson Hyperlegible", -apple-system, sans-serif`, 16px (user had that pref selected). |
| Monospace | `--sw-font-mono` | `'JetBrains Mono', ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace` | User-pickable via `[data-mono]`: `system`, `jetbrains`, `cascadia`. Used for IDs, timestamps, kbd badges. |
| Brand wordmark only | `--sw-font-brand` | `'Oleo Script', 'Brush Script MT', cursive` | Fixed - does NOT track the Font Family preference. Sidebar wordmark only. |

**Principle: exactly one CSS custom property gates each font role** (`--sw-font-sans` / `--sw-font-mono` / `--sw-font-brand`), rebindable via a `data-*` attribute on `<html>`. Never hardcode a `font-family` inline; reference the token so a user preference (or a brand swap for Canticle) changes every consumer at once.

### Type scale

**Intended target (M55):** a clean 13 / 14 / 16 / 18 / 20px ramp, driven by the Font Size preference:

```css
[data-font-size="small"]    { font-size: 13px; }
[data-font-size="medium"]   { font-size: 14px; }  /* default */
[data-font-size="large"]    { font-size: 16px; }
[data-font-size="x-large"]  { font-size: 18px; }
[data-font-size="xx-large"] { font-size: 20px; }
```

**Known debt - current drift (do not port these values):** the #1894 rendered font-size sweep (getComputedStyle over every element, Dashboard + Artists list) found sizes off that clean scale: **9px, 10px, 11px, 12.576px, 13.712px, 14.864px, 28px** alongside the intended 13/14/16/18/20/30px values. The `.576/.712/.864` fractional pattern looks like a `rem`-based multiplier drifting off whole pixels (e.g. a `0.786rem`-style token), not deliberate design - flagged in the pre-flight as "worth tracing to its source class/token before the F3 fix, since it may be one shared utility class used in many places." **This is technical debt to fix, not a pattern to reproduce in Canticle.** *Resolved (partly) under #1894 F3: the raw-px `font-size` declarations on the artist-detail (`.sw-next-artist-detail`) and reports (`.sw-rep-*`) surfaces were converted to the pref-aware rem scale (render-preserving at the 14px medium anchor, so they now track all five `[data-font-size]` stops). Residual raw-px remains in shared cross-surface chrome (context menu, filter flyout, bottom tab, sidebar wordmark) - flagged for a scoped follow-up.*

Headline sizes seen: H1/hero name (`.sw-next-hero-name`) 30px / weight 700. Card head `h2` 14px / weight 600 (`--sw-type-section` token). Meta text 12px (`--sw-type-meta`).

### Weights

- Body: 400 (Inter/Atkinson regular)
- H1 / hero: 700
- Card headers, button labels: 500-600
- No evidence of a 300/light weight in active use.

### Case convention

- **Headings and column headers: Title Case.**
- **Body copy: sentence case.**
- No BCP-47 locale codes surfaced in UI copy (e.g. no "en-US" strings shown to the user).
- No CTA/hero marketing jargon - copy is plain and functional (`en.json` samples: "Add", "Apply", "Cancel", "Confirm", "Save anyway").

---

## 3. Color

### Surface tiers (dark, default theme - from `design-tokens.css` `:root`/`.dark`)

| Token | Value | Use |
|---|---|---|
| `--sw-sidebar-bg` | `#0f172a` (slate-900) | Sidebar base |
| `--sw-content-bg` | `#0f172a` (dark) / `#f8f9fa` (light) | Page background |
| `--sw-content-text` | `#e2e8f0` (dark) / `#1e293b` (light) | Primary text |
| `--sw-content-text-secondary` | `#94a3b8` (dark) / `#64748b` (light) | Secondary/meta text |
| `--sw-glass-bg` | `rgba(30, 41, 59, 0.85)` (dark) / `rgba(255, 255, 255, 0.85)` (light) | Card/chrome tint; alpha driven by the user's Background-Opacity preference (20-100%, default 85%) |
| `--sw-glass-bg-light` | `rgba(255, 255, 255, 0.75)` | Explicit light-glass variant token |
| `--sw-glass-border` | `rgba(255, 255, 255, 0.1)` (dark) / `rgba(0, 0, 0, 0.10)` (light) | Card hairline border |
| `--sw-glass-blur` | `blur(32px) saturate(150%)` | True blur - used by chrome surfaces, NOT by `.sw-dash-card` (see §4) |

Rendered cross-check (#1894, dark, `data-theme="dark"`): `--sw-glass-bg: rgba(30, 41, 59, 0.85)` confirmed - "a genuine dark navy, not the light-glass trap."

There is a **second, next/-specific token layer** (`--swd-*`, defined in a scoped block inside `input.css` around line 3707) that dashboard/artist-detail cards actually consume:

| Token | Value | Notes |
|---|---|---|
| `--swd-surface` | `var(--sw-glass-bg)` | Aliases the shared glass token - same color pipeline, different name for the "prototype card" system |
| `--swd-bg-base` | `#f1f5f9` (slate-100) | Light-mode faint base behind cards |
| `--swd-bg-raised` | `#ffffff` | Light-mode card |
| `--swd-bg-elev` | `#f8fafc` | Row hover / popover |
| `--swd-line` | `rgba(15, 23, 42, 0.10)` | Card border (light); dark value is theme-flipped elsewhere |
| `--swd-line-strong` | `rgba(15, 23, 42, 0.16)` | Stronger border variant |
| `--swd-ink` | `#0f172a` | Primary ink (light mode) |
| `--swd-ink-2` | `#334155` | Secondary ink |
| `--swd-ink-3` | `#64748b` | Quiet meta/label ink - AA on white only, flagged as borderline (~4.8:1) |

**Portability note:** for Canticle, `--swd-*` can likely be dropped and everything routed through the single `--sw-*` layer - the two-layer split is Stillwater-specific historical accretion (the `--swd-surface: var(--sw-glass-bg)` alias comment literally says it exists to let older "prototype card" CSS consume the shared token transparently). Don't port the duplication; port the single-source-of-truth intent.

### Accent

| Token | Value | Where it's allowed |
|---|---|---|
| `--sw-accent-primary` | `#3b82f6` (blue-500) | Sidebar active-link background (`rgba(59,130,246,0.15)` dark / `.1` light) and active-link text (`#93c5fd` dark / `#1e40af` light, tuned specifically for WCAG AA over the translucent bg - see design-tokens.css:110,180,194). Also used for focus rings and the one restyled-checkbox `:checked` fill (`.sw-next-artists input[type=checkbox]`, see §5). |
| `--sw-accent-primary-glow` | `rgba(59, 130, 246, 0.3)` | Glow/shadow accents (e.g. toggle focus) |

**Rendered confirmation (#1894):** sidebar active link is the *one clear place blue/accent shows up* on the artist-detail and dashboard pages - `bg: rgba(59,130,246,0.15)`, `color: rgb(147,197,253)`. No primary-CTA button exists to contrast against it (see §1.2).

### Severity / status colors (semantic, not decorative)

| Token | Ink | Background | Meaning |
|---|---|---|---|
| `--sw-severity-error` | `#f87171` | `rgba(248,113,113,0.12)` | Error |
| `--sw-severity-warning` | `#fbbf24` | `rgba(251,191,36,0.12)` | Warning / locked-by-user |
| `--sw-severity-info` | `#818cf8` | `rgba(129,140,248,0.12)` | Info / locked-by-import |
| `--sw-success` | `#4ade80` | `rgba(74,222,128,0.12)` | Success / synced |

Pattern: **ink-on-tinted-background pill**, never a solid fill, for all inline semantic status (see `SeverityBadge`, `LockBadge`, `SyncBadge` in `web/components/badge.templ`). Deliberately avoided for one specific case: the sidebar's update-count pill is "a neutral blue rather than red" by design because it is *not* a danger/alarm state (`input.css:485` comment) - i.e. color choice encodes semantic urgency, not just "a badge exists here."

### Danger / destructive color (irreversible operations)

Two values are in live use and they **disagree** - flag this as a real inconsistency to resolve, not a menu to pick from freely:

| Value | Where | Notes |
|---|---|---|
| `oklch(0.577 0.245 27.325)` | Merge-modal **Confirm merge** button (rendered, `artist_duplicates.templ`) | A saturated red, `border-radius: 4px`, white text, 14px/weight 500 |
| `bg-red-600` / `hover:bg-red-700` (Tailwind, ≈ `#dc2626`/`#b91c1c`) | `ConfirmModal` component's default Confirm button (`web/components/confirm_modal.templ:51`), bulk-delete buttons (`artist_duplicates.templ:117,475`) | `border-radius` unspecified → Tailwind default (4px via `rounded`) |
| `--sw-danger` (custom property, referenced with a `#ef4444` fallback) | `input.css:6343` | A named danger token exists but is only referenced once in the file grepped - not the dominant pattern yet |
| `--flyout-danger-ink` / `--flyout-danger-bg` / `--flyout-danger-border` | Rule-engine "fix-all" row (`rgba(239,68,68,...)` family, red-500) | Explicitly "muted danger tone (never a solid red/blue fill)" per the code comment - a *different* visual language (subtle tint) from the modal buttons' solid fill |
| Cancel button (paired with the above) | `oklch(0.872 0.01 258.338)` text / `#a1a1aa`-ish (Tailwind `text-gray-700`/`dark:text-gray-300`) on transparent | Consistent across both modal variants |

**Recommendation to record as the target rule, not yet fully true today:** destructive/irreversible confirm actions get a solid red fill (`bg-red-600` Tailwind family is the more common of the two, and is a named, greppable Tailwind color rather than an ad-hoc `oklch()` literal - prefer standardizing on it); non-blocking/inline danger *state* (e.g. a flagged row, a fix-all rule count) gets the muted tint pattern (`--flyout-danger-*` style), never a solid fill. **Known debt:** the merge modal's `oklch(...)` literal should be reconciled to the same red family as `ConfirmModal`'s `bg-red-600` rather than left as a second, slightly different red.

### Modal button radius mismatch (debt)

Merge-modal / `ConfirmModal` buttons render at `border-radius: 4px`; the artist-detail/dashboard ghost toolbar buttons use `--sw-radius-sm` = `6px`. Flagged in the #1894 pre-flight as "a small but real motif deviation worth folding into the #1894 button-consistency pass." **Target: standardize all buttons on `--sw-radius-sm` (6px).**

---

## 4. Surfaces & Elevation

### The glass-card pattern - `.sw-dash-card`

Canonical card surface for dashboard + artist-detail content. Rendered tokens (dark, #1894 pre-flight, confirmed via `getComputedStyle`):

| Property | Value |
|---|---|
| `background` | `rgba(30, 41, 59, 0.85)` (via `--swd-surface` → `--sw-glass-bg`) |
| `border` | `1px solid rgba(148, 163, 184, 0.14)` |
| `border-radius` | `14px` (`var(--sw-radius-lg)`) |
| `backdrop-filter` | `none` - **the glass tint is a flat rgba, not an actual blur.** Do not assume `.sw-dash-card` blurs whatever is behind it. |
| `box-shadow` | soft (`var(--swd-shadow-1)`, value not captured this pass - `TODO: confirm exact shadow value`) |

Sub-structure: `.head` (flex row, 12px gap, `14px 18px` padding, bottom hairline border, `h2` 14px/600 + right-aligned `.meta` 12px secondary-ink) / `.body` (`16px 18px` padding, or `8px` with a `.tight` modifier) / `.foot` (centered flex, `10px 18px` padding, top hairline border).

Hero variant (`.sw-next-hero`, artist name header) uses the same surface family but slightly more opaque: `color(srgb 0.0667 0.102 0.180 / 0.92)`.

**Important exception - the merge modal does NOT use this pattern.** Its `<dialog>`/wrapper is `background-color: rgba(0,0,0,0)` (fully transparent), `backdrop-filter: none`, at the outer frame level - the modal's visible surface comes from a nested child, not the container. This is inconsistent with `.sw-dash-card` and is flagged as a gap in the pre-flight, not a second sanctioned pattern.

### True-blur glass - `.sw-glass` / `.sw-card`

For chrome (sidebar, bottom bars, filter flyout, command palette popovers): real `backdrop-filter: blur(18px-32px) saturate(150-180%)`, `border: 1px solid rgba(255,255,255,0.1-0.28)`, soft box-shadow. This is the OTHER surface family - reserve it for floating/overlay chrome, not for in-page content cards (which use the flat-tint `.sw-dash-card` family instead).

### Radius scale

| Token | Value | Applies to |
|---|---|---|
| `--sw-radius-sm` | 6px | Buttons (toolbar ghost buttons), small controls |
| `--sw-radius-md` | 10px | Mid-size surfaces (popovers, segmented controls) |
| `--sw-radius-lg` | 14px | Cards (`.sw-dash-card`), drawers |
| `--sw-radius-xl` | 20px | Larger sheet/drawer corners |

Known debt: modal buttons currently render 4px, off this scale (see §3).

### Spacing / density

User-controlled via `[data-density]` = `compact` / `comfortable` (default) / `spacious`, driving a family of `--sw-density-*` tokens (row padding, card padding, tile gaps, feed-row padding, artist-detail inter-card gap). `compact` is deliberately a passthrough to the original hardcoded spacing rather than its own token set - "today's native hardcoded spacing IS compact" (design-tokens.css:243-247 comment) - so `comfortable`/`spacious` are the only two states that actually apply the `--sw-density-*` tokens.

---

## 5. Components

| Component | Class / pattern | When to use | Notes |
|---|---|---|---|
| **Buttons (toolbar/ghost)** | transparent bg, `1px solid rgba(148,163,184,0.14)`, `border-radius: 6px` (`--sw-radius-sm`), `color: rgb(182,194,212)`, 12-14px/weight 500 | All top-bar / toolbar actions (Edit, Actions, Refresh, Run Rules) | **No primary/filled button exists in this family.** Every action button is visually equal weight. |
| **Buttons (danger/confirm)** | solid red fill, white text - see §3 danger-color table for the two live variants | Irreversible confirm actions inside modals only (Merge, Delete) | Currently 4px radius (debt - target 6px); reconcile the two red values (debt). |
| **Buttons (modal cancel)** | transparent bg, secondary-ink text, 4px radius, 14px/weight 500 | Modal dismiss/cancel | Radius debt, same as above. |
| **Pills / badges** | `inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium`, ink-on-12%-opacity-background of the relevant semantic color | Status indicators: severity, lock source, sync state, platform source, health score | Always paired with `role="img"` + `aria-label` (or `role="status"`/`aria-live` for the syncing spinner state) - never color-only. `web/components/badge.templ` is the canonical source for this pattern; reuse it rather than inventing a new pill style. |
| **Count pill (sidebar)** | `border-radius: 9999px`, `--sw-sidebar-count-pill-{bg,fg,border}` tokens (neutral blue, not red) | Notification/update counts | Deliberately non-alarming color even though it's a "count" badge - semantic urgency, not just decoration (input.css:485). |
| **Toggles (iOS-style)** | `Toggle()` component: `.sw-toggle-wrapper` / `.sw-toggle-input` (visually hidden, `role="switch"`) / `.sw-toggle-track` (44×24px pill) / `.sw-toggle-knob` (20×20px, slides via `transform: translateX`) | Any boolean setting/preference | Checked state = `--sw-accent-primary` track fill. Respects `prefers-reduced-motion` and the app's `[data-motion="on"]` override. This is the **established boolean-control pattern** - use it instead of a bare checkbox for on/off settings. |
| **Checkboxes (established, styled)** | `.sw-next-artists input[type="checkbox"]` restyle (`appearance:none`, 1rem×1rem, 4px radius, `1.5px solid #cbd5e1`/`#475569` border, tinted `#f1f5f9`/`#1e293b` background - never plain white; `:checked`/`:indeterminate` → `#2563eb` fill + drawn check/dash glyph; `:hover` → blue border; `:focus-visible` → `outline: 2px solid #3b82f6`) | Row-selection checkboxes (bulk-select in artists table/grid) | This is the pattern to reuse anywhere a styled multi-select checkbox is needed. A de-blue variant exists for non-selection uses (column-visibility toggle: transparent fill, theme-ink border/glyph instead of blue - `input.css:3252-3285`) - use that variant when the checkbox is a settings affordance rather than a row selection. |
| **Checkboxes (merge modal - resolved in #1894 F2)** | Merge modal's "Include in merge" checkboxes, `.sw-merge-include` | Multi-select "keep this record in the merge" | The pre-fix state was a native anti-pattern (bare `type="checkbox"`, `appearance: auto`, no accent override): because native rendering differs only by the `disabled` attribute, the locked-in survivor rendered gray/dim while every discard rendered blue/"on" - inverted. An interim fix over-corrected by filling every checked box solid blue-600, which is off-motif (no solid-blue fills in this system). **Sanctioned pattern (shipped): the neutral de-blue treatment** - mirror `.sw-next-artists [data-col-toggle]` (§ below): transparent box in all states, thin themed border, checkmark ink = `var(--sw-content-text)`; the force-checked, `disabled` survivor is muted (`var(--sw-content-text-secondary)` + `opacity: 0.6`) so it reads as "locked-in", never a color inversion or a blue fill. All colors are themed `--sw-*` tokens (both themes). The kept-vs-discarded meaning is carried by the **row** (a "Keeping" badge + accent survivor row / discard danger tone), not the checkbox color. **Rule: never leave a real selection checkbox at browser-default styling; do not use a solid accent fill to mean "selected" - use the neutral drawn-glyph treatment.** |
| **Radios** | Survivor-selection radio in the merge modal (`.sw-merge-radio`, styled `appearance: none`) | Pick which record survives | Styled to match the checkbox box geometry; its `:checked` fill was still accent-blue at capture, pending a maintainer decision on whether to de-blue it to match the neutral checkboxes. Treat as an open reconciliation question, not a sanctioned "blue radio" pattern. |
| **Accordions / disclosure** | Native `<details>`/`<summary>`, e.g. discography sort dropdown (`artist_detail_sections.templ:92-93`, `list-none cursor-pointer inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-md border border-[var(--swd-line)] ... focus:ring-2 focus:ring-blue-500`) and activity-feed detail expanders (`activity.templ:377-378`). A separate custom accordion also exists for artist-detail sections, driven by `aria-expanded` + `.sw-section-collapse-btn`/`.sw-section-chevron` (`input.css:3950-4006`) - chevron rotates, body is toggled via the `aria-expanded` attribute selector rather than JS class toggling. | Any collapsible section/dropdown | Prefer native `<details>` where the browser's built-in semantics suffice (keyboard, focus, no-JS fallback for free - matches the minimal-JS principle); reserve the `aria-expanded`-driven custom pattern for cases needing finer control (e.g. syncing collapse state with density/animation prefs). |
| **Links** | plain text, no border, secondary-ink color (`rgb(182,194,212)` dark), `hover:underline` | Inline navigation (e.g. "Findings" link on artist-detail) | 12px in the observed instance. |
| **Cards** | `.sw-dash-card` - see §4 | Dashboard tiles, artist-detail sections | Do not use the true-blur `.sw-glass`/`.sw-card` classes for in-page content cards; reserve those for chrome. |
| **Modals** | Structural pattern seen in `ConfirmModal`: fixed-position wrapper (`role="alertdialog"`, `aria-modal="true"`, `aria-labelledby`/`aria-describedby`), separate backdrop div (`bg-black/50`), centered content panel (`rounded-xl`, `shadow-2xl`, `ring-1`), footer button row (`flex justify-end gap-2`, top hairline border) | Any confirm/blocking dialog | The merge modal's outer-frame-transparent structure (§4) deviates from this - worth reconciling so every modal's outer wrapper carries the same surface treatment as `ConfirmModal`'s content panel, not just its inner content. |

---

## 6. Copy Conventions

- **Headings / column headers: Title Case.** Body copy: sentence case (e.g. `actions.save_anyway = "Save anyway"`, not "Save Anyway").
- **US English** spelling and terminology throughout `en.json`.
- **No BCP-47 locale codes** exposed in the UI (locale selection uses human-readable names, not raw tags like `en-US`).
- **No CTA/hero marketing jargon.** Copy is short, plain, functional: `Add`, `Apply`, `Cancel`, `Confirm`, `Copy`, `Import`, `Save`. No "Get Started", "Unlock", "Supercharge"-style language anywhere sampled.
- **No invented UI affordances.** Every label/status shown to the user should trace to `en.json` or real API/data state - don't author new banner copy or badge text ad hoc; add it to the locale file and reference it via `t(ctx, "...")`.

---

## 7. Evidence Standard (seed for an automated UX gate)

Binding rule already enforced by process (not yet by tooling) in this repo: **a visual/CSS change is only "done" when confirmed against the LIVE rendered page at the branch HEAD** - never from static inference (reading the CSS source, grepping a class name, or doing contrast arithmetic by hand). Concretely, "done" requires:

1. **Selector match count** - confirm the CSS rule you wrote actually matches an element in the live DOM (a selector can exist and simply never match anything, e.g. a stale ancestor scope).
2. **Computed style** - `getComputedStyle()` on the real element, not the authored CSS value (cascade/specificity/inheritance can silently override the source).
3. **Rendered contrast** - compute contrast ratio from the actual rendered RGB values (foreground vs. background as painted), not from the token's nominal hex.
4. **Screenshot** - visual proof, full-page, in the theme(s)/preference states relevant to the change.

This is exactly how the merge-modal checkbox bug (§5) was actually found and precisely characterized (the `disabled`-attribute-driven native-rendering difference would not have been visible from reading the template's class list alone - `"mt-1 shrink-0"` gives no hint that Chromium renders `disabled + checked` as gray and `enabled + checked` as blue).

**For an automated gate, encode as CI/pre-merge checks:**
- Render the affected page(s) headless (e.g. Playwright) at the branch HEAD, in both themes and at minimum one non-default preference combination (font size, density, background opacity) per changed surface.
- Assert selector match counts > 0 for any new/changed CSS rule scoped to that page.
- Assert `getComputedStyle()` values for the specific properties the change targets (not a full diff - targeted assertions tied to the change).
- Run a real accessibility scanner (axe-core) against the rendered page, not a hand-rolled contrast check - the repo's existing convention is "real axe-core on the live page," full-page, both themes.
- Do not treat "the source diff looks right" as a passing signal by itself.

---

## 8. Portability Notes for Canticle

**Swap these (Stillwater-brand-specific):**
- `--sw-font-brand` (Oleo Script wordmark) and `--sw-brand-color` (`#2563eb`) - the brand identity layer.
- `--sw-accent-primary` (`#3b82f6`, blue) - pick Canticle's own accent hue, but **keep the *rule* that it appears in exactly one place** (active-nav state) rather than scattered through primary buttons.
- Sidebar/dashboard-specific pixel widths (`--sw-sidebar-width-full: 220px`, `--sw-sidebar-width-icon: 56px`) and the `--sw-content-max-width: 1200px` layout constant - these are Stillwater's chosen proportions, not structural requirements.
- Platform badge colors/icons (Emby green, Jellyfin blue, Lidarr green, filesystem gray) - entirely Stillwater-domain (media-platform integrations); Canticle will have its own integration set.
- The specific font choices (Inter / Atkinson Hyperlegible / JetBrains Mono / Cascadia Code / Oleo Script) - keep the *mechanism* (one swappable token per role, user-pickable via `data-*` attribute), not necessarily these exact faces.

**Keep these (structural rules, not values):**
- The single-CSS-custom-property-per-role token architecture (`--sw-font-sans`, `--sw-font-mono`, `--sw-accent-primary`, etc.) rebindable via `data-*` attributes on `<html>` - this is what makes user preferences AND brand swaps both trivial and centralized. Don't hardcode any color/font/radius/spacing value directly in a component; always go through a token.
- Dark-as-default with a full light override block, never the reverse.
- The "no primary CTA button" restraint principle - ghost-style toolbar buttons, primary/secondary contrast reserved for modal-internal cancel/confirm pairs only.
- The flat-tint-card vs. true-blur-chrome surface split (§4) - two distinct, intentional surface families, not an accident to merge.
- Ink-on-tinted-background pill pattern for all semantic status, always paired with a non-color accessible signal (`aria-label`, `role="img"`/`role="status"`).
- The styled-checkbox patterns (`appearance:none` + a custom drawn `:checked`/`:indeterminate`/`:focus-visible` glyph) as the only sanctioned checkbox treatments - never ship a bare native checkbox for a real selection control, especially not a `disabled` one that needs to communicate "chosen". There are two sanctioned variants, chosen by context: an **accent-fill** variant for high-emphasis row/bulk selection (`.sw-next-artists`), and a **neutral de-blue** variant (transparent box + themed-ink glyph, `[data-col-toggle]` / merge-modal §5) for settings affordances and anywhere a solid accent fill would be off-motif. Pick the neutral variant by default; reserve the accent fill for genuine row-selection emphasis.
- Title-Case-headings / sentence-case-body, no jargon, no invented copy - copy sourced from a single locale file, not ad hoc strings in templates.
- The rendered-evidence standard (§7) as a process/gate requirement, independent of which project it's applied to.
- Radius and density scales as small, closed token sets (`--sw-radius-{sm,md,lg,xl}`, `--*-density-*`) rather than ad hoc per-component values - even though Stillwater's own codebase currently violates this in places (modal 4px radius vs. the 6px scale), the *target* architecture is the thing to port, not the current drift.

**Debt not to inherit (fix-before-port or port-as-fixed):**
- The off-scale fractional font sizes (§2) - port the intended 13/14/16/18/20 scale, not the `.576/.712/.864` drift.
- The two disagreeing danger-red values and the 4px-vs-6px button radius split (§3) - pick one of each for Canticle from day one.
- The `--sw-*` / `--swd-*` duplicate token layer (§3) - Canticle should have one token layer, not two that alias each other.
- The merge-modal's checkboxes are resolved under #1894 F2 (the neutral de-blue treatment, §5) - port that fix, not the pre-fix native/blue states. The survivor radio's blue fill and the transparent outer-modal-frame (§4) remain open reconciliation items at capture.

---

## 9. Sidebar

Source: `web/templates/sidebar.templ` (canonical, promoted M55 #1757 PR-1), `web/static/js/sidebar.js`, sidebar CSS in `input.css` (~lines 143-900).

### Structure (top to bottom, frequency-sorted)

1. **Brand header** - Oleo Script wordmark (`@components.Wordmark`) + muted mono version line (`v{version}`) + collapse-chevron button, all in one `.sw-sidebar-header` row.
2. **Primary destinations** - Dashboard, Artists. Each link carries an HTMX-hydrated count badge (`hx-get=".../badge" hx-trigger="load, every 30s|120s"`).
3. **Reports section** - a `<span class="sw-sidebar-section-label">` acts as a **header, not a link** ("Reports" is not itself clickable - a deliberate M55 #1757 fix that killed a prior Reports/Compliance duplication). Contains: Reports workspace link, then role-gated items (Compliance/Duplicates/Foreign Files for admins with HTMX-hydrated count pills; a plain Compliance link for non-admins with no polling, to avoid poll-and-403 against admin-only count endpoints).
4. **Activity** - its own section, single link, no count badge.
5. **Spacer** (`<div class="flex-1">`) - pushes everything below it to the bottom of the sidebar regardless of content height.
6. **Low-frequency destinations** - Logs (admin-only), Settings (admin-only, carries an update-available dot), Preferences (opens the Preferences **flyout drawer**, not a page navigation - `data-sw-prefs-trigger` + `aria-haspopup="dialog"` + `aria-controls="sw-prefs-drawer"`; the `href="/preferences"` is a no-JS progressive-enhancement fallback only).
7. **Bottom bar** - a divider, then a `role="group"` row of three glyph-only icon buttons (theme cycle, help/shortcuts, logout), then the user-identity row (avatar + name/role), which is **also** a third entry point into the Preferences flyout.

**Principle: role-gating avoids "poll-and-403."** Items whose count endpoint 403s for non-admins are omitted entirely for non-admin sessions rather than rendered with a suppressed/broken pill - "no lying markup" (code comment, sidebar.templ:34-36). Port this rule: never render a placeholder that will silently fail to hydrate for the current user's role.

### Active-link treatment (the one accent, §3)

`.sw-sidebar-link-active` → `color: var(--sw-sidebar-active-text)` on `background: var(--sw-sidebar-active-bg)` (`rgba(59,130,246,0.15)` dark / `.1` light; text tuned per-theme for WCAG AA - see §3). This is confirmed (§3, rendered pre-flight) as the one place blue/accent shows up outside of danger buttons. Active state is driven by comparing the link's `data-path` attribute to the current route (helper logic in `sidebar_helpers.go` / `sidebar.js`, not scanned line-by-line this pass - `TODO: confirm exact match algorithm, e.g. prefix vs. exact`).

### Collapse / responsive states

Three states via `data-sidebar-state` on `#sw-sidebar`: `full` | `icon-only` | `hidden`. Set initially by `preferences.js` from the user's saved sidebar preference; toggled at runtime by `swSidebar.cycle()` (bound to the header's collapse-chevron button and to a separate `#sw-sidebar-restore` button that appears - via a `.sw-sidebar-restore-hidden`/shown class toggle - only when the sidebar is fully `hidden`).

- `icon-only` / `hidden`: `.sw-sidebar-label`, `.sw-sidebar-section-label`, `.sw-sidebar-user-info`, `.sw-sidebar-kbd`, `.sw-sidebar-badge`, `.sw-sidebar-version`, `.sw-sidebar-logout-btn` all collapse (hidden/width-0 via CSS, not removed from the DOM) - icons remain.
- `icon-only`: sub-elements like `.sw-sidebar-header`, `.sw-sidebar-brand`, `.sw-sidebar-user` get dedicated compact layouts (not just their children hidden).
- The bottom action row (`.sw-sidebar-actions`) switches from `space-evenly` horizontal to a vertical stack in `icon-only`/`hidden` (CSS-only, no JS layout logic).

**Portability note:** the three-state model (not just a binary collapse) is a structural win worth keeping - `hidden` fully reclaims the sidebar's width (a true "distraction-free" mode) while `icon-only` keeps navigation reachable. A pure show/hide toggle loses that middle state.

---

## 10. Keyboard Shortcuts

Source: `web/static/js/keyboard.js` (single IIFE, exported as `window.swKeyboardShortcuts`), `web/components/cheat_sheet_modal.templ`.

**Scope:** the full shortcut model below is **next/-only**. It is gated by `isNextPage()` (checks the current pathname against the base-path meta tag) and is explicitly "inert until a screen declares `data-sw-*` attributes, so the stable channel ... is unaffected" (keyboard.js:1-5). The stable channel has its own, separate, unrelated `?`-key handler.

### The model: a live registry, not a hardcoded list

`keyboard.js` maintains a `registry` array rebuilt from the **live DOM** on every load and every HTMX swap (`htmx:afterSwap`, `htmx:load` → `rebuild()`). Three layers feed it:

1. **Page-level action keys** - any element with `data-sw-shortcut="<key>"` (+ optional `data-sw-shortcut-label`, `data-sw-scope`) is auto-discovered; e.g. `artist_detail.templ:361-362` declares `data-sw-shortcut="r"` for the Refresh Metadata button.
2. **Roving-list descriptors** - a single `[data-sw-roving-list]` container with `[data-sw-roving-item]` children gets `j`/`k`/`Enter` auto-registered, using per-list `data-sw-roving-label-{j,k,Enter}` attributes for the labels shown in the cheat sheet.
3. **Manual entries** - `swKeyboardShortcuts.register(scope, entries)` for shortcuts implemented by inline scripts that aren't DOM-discoverable (e.g. the global `g d` / `g a` / `⌘K` / `?` / `Esc` set registered at the bottom of keyboard.js:690-701).

**This is the truth-check rule the maintainer asked for: the cheat sheet is not a separately-authored document.** `CheatSheetModal`'s `renderContent()` calls `window.swKeyboardShortcuts.list()` fresh **every time it opens** (cheat_sheet_modal.templ:109-137) - it can never drift from the actual bindings because it has no independent source of truth. **Any UX gate checking "does the shortcut legend match reality" should assert this invariant holds** (the registry is DOM-derived, not hand-maintained) rather than diffing a static legend against a static handler list.

### Key bindings (confirmed from source)

| Key(s) | Action | Scope |
|---|---|---|
| `j` / `k` | Move roving focus down/up one item in the active list | List-navigation (roving) |
| `Enter` | Activate the currently roving-focused item (clicks its `[data-sw-roving-activate]` target) | Roving |
| `h` / `l` | Jump a whole page (previous/next) - **opt-in**: only active when the roving list declares `data-sw-roving-boundary-{prev,next}` (a CSS selector for its pager controls); free for a screen's own use otherwise | Roving (paged lists only) |
| *(seamless boundary)* | Pressing `j` at the last item / `k` at the first item, on a boundary-opted-in list, clicks the pager and re-seats focus at the new page's first/last item - vim-style continuous scroll across pages | Roving |
| `/` | Focus the content search box (`data-sw-shortcut="/"`) | Page action |
| `s` | Focus a secondary "rail" search (reports side-rail) - falls through (does not consume the key) if no `[data-sw-shortcut="s"]` target exists on the page | Page action |
| `f` / `F` | Toggle the filter flyout open/closed via `activateToggle()` (uses the `aria-expanded`/`aria-controls` contract so a second press closes it, not just re-opens) | Page action |
| `r` / `R` | Fire a page's primary refresh/run action; checks for an exact-case target first (so a page can register both `r` = Refresh and `R` = Run Rules distinctly), else falls back to lowercase `r` | Page action |
| `g` then `{d,a,r,l,f,s}` | **Leader-key navigation** (vim-style): pressing `g` arms a 1.5s window (`LEADER_TIMEOUT_MS`, test-overridable via `window.SW_LEADER_TIMEOUT_MS`); the next key navigates: `d`→Dashboard, `a`→Artists, `r`→Reports, `l`→Logs, `f`→Reports (Findings), `s`→Settings. Suppressed while the cheat-sheet modal is open. | Global |
| `⌘K` / `Ctrl+K` | Toggle the command palette | Global |
| `?` | Open the keyboard-shortcut cheat sheet (`CheatSheetModal`) | Global |
| `Esc` | Layered: (1) hide the command palette if open, (2) close the cheat sheet if open, (3) blur the search box if it's focused (preserving the typed query - `preventDefault()` suppresses the native "clear on Escape" so exit ≠ reset), (4) clear a bulk-selection if any checkboxes in a `[data-sw-bulk-scope]` are checked | Global / contextual |
| `⌘A` / `Ctrl+A` | Select all rows within `[data-sw-bulk-scope]` | Bulk-select |
| *(per-list contextual key)* | One additional key per screen, declared via `data-sw-roving-context-key` on the roving list (e.g. a kebab-menu key), dispatched to a registered `onContext()` handler or the focused item's `[data-sw-roving-context]` target | Roving (per-screen) |

### Guardrails baked into the dispatcher

- **`isTyping()` guard**: shortcuts are swallowed while focus is in a real text field (`TEXTAREA`, `SELECT`, `contentEditable`, or a text-like `INPUT`) - but a focused checkbox/radio does **not** count as typing (Firefox lets rows take focus), so `j`/`k` keep working after clicking a row checkbox.
- **`reservedKey()` collision guard**: a page declaring its own contextual key gets a `console.warn` if that key collides with `j`/`k`/`Enter`/an already-declared page action - a dev-time signal against silently shadowed bindings, not a hard error.
- **`warnAdvertisedMissing()`**: if the registry advertises a key (so the cheat sheet lists it) but its DOM target has since disappeared (e.g. a conditionally-rendered button), pressing the key logs a console warning rather than failing silently - "keys not in the registry stay genuinely inert ... only advertised-then-missing keys get a dev breadcrumb."
- **Platform-aware modifier glyph**: on macOS, every `.sw-mod-key` chip is swapped from the default "Ctrl" text to "⌘" at init (`applyPlatformGlyph()`), non-fatally (a thrown error just leaves the Ctrl default).
- **Boundary-intent cleanup on error**: if a paging-boundary click's HTMX request errors (`htmx:responseError`/`htmx:sendError`), the pending "seat focus at first/last item of the new page" intent is cleared so a later, unrelated swap doesn't wrongly steal focus.

### The cheat-sheet legend UI

`CheatSheetModal` (`role="dialog"`, `aria-modal="true"`, full Tab/Shift-Tab focus trap, focus-restore-to-opener on close) groups the live registry into three labeled sections in a fixed order: **Global** (manual entries) → **Page Actions** (`action` kind) → **List Navigation** (`roving` kind). Each row renders as `<dt>label</dt><dd><kbd class="sw-kbd">key</kbd></dd>`. Mounted only in `LayoutNext` (absent from stable pages), uses `.sw-glass` (true blur, not `.sw-dash-card`'s flat tint - see §4/§9's chrome-vs-content distinction), and its open/close is an instant class toggle (no CSS animation) so `prefers-reduced-motion` needs no special-casing.

**Portability:** the registry-driven, DOM-derived cheat sheet is the single most reusable interaction pattern in this spec for a UX gate - a Canticle port (or an automated conformance check) should assert every interactive shortcut carries a `data-sw-shortcut`/`data-sw-roving-*` declaration rather than a bare, undiscoverable `keydown` listener, specifically so the legend can never drift from reality.

---

## 11. Flyouts, Dropdowns, Menus, Popovers

Four distinct overlay patterns exist, each for a different job - do not treat them as interchangeable:

### Filter flyout (`FilterFlyout` component, `filter_flyout.templ` + `filter-flyout.js`)

A slide-in panel from the **right edge** of the viewport (`.sw-filter-flyout`, `bg-white dark:bg-gray-800` - solid, not glass). Structure: scrim (`.sw-filter-scrim`, click-to-close) + `<aside role="region">` with header (title + optional help + close button), scrollable body (`.sw-scroll`, caller-supplied `FilterSection` blocks), footer (apply/clear, active-filter-count badge shown only when > 0). Closed state carries `aria-hidden="true" inert` on the panel (not just a CSS hide) so it's fully unreachable to AT/keyboard when shut. Opens via `f`/`F` keyboard shortcut (through `activateToggle()`, §10) or its trigger button; the trigger id is threaded through so focus returns to it on close (`data-trigger-id`). Respects reduced-motion and lite-mode (`input.css:1572-1579`).

### Context menu / kebab menu (`ContextMenu`/`ContextMenuList`, `context_menu.templ`)

A "..." (ellipsis) trigger that renders **two different overlays depending on viewport**, both driven by one `ToggleContextMenu(id)` script:
- **Desktop (≥768px):** a dropdown panel anchored `absolute right-0 top-full`, `role="menu"`, `rounded-md border ... shadow-lg`. Opening moves focus to the first `[role="menuitem"]`.
- **Mobile (<768px):** the *same* trigger instead opens a **bottom sheet** (`.ctx-bottom-sheet`) with a scrim, a drag-handle bar, the same item list, and an explicit Cancel button footer. Body scroll is locked (`ctx-sheet-body-lock`) while open; opening moves focus into the sheet after a 300ms transition delay, closing returns focus to the trigger.

Both variants share: only one context menu open at a time (opening any menu closes all others - `ToggleContextMenu` walks the DOM and force-closes every other open panel/sheet first), global click-outside-to-close and `Escape`-to-close (`ContextMenuGlobalJS`, mounted once in the layout), a focus trap on Tab/Shift-Tab while the mobile sheet is open, and auto-close when an HTMX request fires from inside the menu (`htmx:beforeRequest`).

**Item styling:** `.context-menu-item` (plain), `.context-menu-item-danger` (red, auto-grouped below a divider - `ContextMenuList` computes `firstDestructiveIndex` and inserts exactly one divider before the first destructive item), `.context-menu-item-disabled` (grayed, `disabled` attribute).

### Command palette (`command-palette.js` - not deep-read this pass, `TODO: confirm full behavior`)

Triggered by `⌘K`/`Ctrl+K` (§10), gated to next/ pages. Referenced via `.sw-cmdk` in the shared `.sw-kbd` selector list (input.css:3334) confirming it shares the same `<kbd>` chip styling as the cheat sheet and sidebar. Toggled via `window.swCommandPalette.toggle()`/`.hide()`, and its open state takes priority over the cheat-sheet Esc-handler (§10 Esc-layering table, step 1).

### Preferences flyout (`prefs-drawer.js` / `.sw-prefs-drawer`)

A right-edge drawer (distinct from the filter flyout, same directional convention) opened from three sidebar entry points (§9: the Preferences nav link, and the user-identity row) via `data-sw-prefs-trigger` + the `aria-haspopup="dialog"`/`aria-controls="sw-prefs-drawer"`/`aria-expanded` contract. Type sizing inside it is driven entirely by `--sw-type-*` rem tokens ("NO hardcoded px font-sizes inside `.sw-prefs-drawer`" - enforced by a code comment, input.css:1828-1829) so it always reflects the live Font Size preference, including while the user is actively changing that preference from inside the drawer itself.

**Shared convention across all four:** trigger carries `aria-haspopup`/`aria-expanded`/`aria-controls`; closed panels are inert (`aria-hidden="true"` + either `inert` or a `hidden`/off-screen CSS state, never merely `display` left ambiguous); opening moves focus in, closing returns focus to the trigger; Escape closes; only one of a given overlay family is open at a time.

---

## 12. Toasts

Source: `web/templates/layout.templ` (inline "Toast Manager" script, ~lines 347-560+), `web/components/error_toast.templ`, `web/static/js/sse.js`.

### Placement & stacking

Fixed container `#error-toast-container`, `top-4 right-4`, `z-50`, `flex flex-col gap-2`, fixed width `w-80` (320px), `role="status" aria-live="polite"`. **Max 3 visible at once**; a 4th+ toast queues and is appended only when a slot frees up (`MAX_VISIBLE = 3`, `queue`/`visible` arrays).

### Grouping

Toasts of the **same type + exact message** arriving within a **2-second window** (`GROUP_WINDOW_MS`) collapse into one toast with an incrementing count badge (`findGroupable()`), rather than stacking duplicates - prevents a burst of identical SSE events from flooding the stack. The auto-dismiss timer resets on each group update.

### Types & color coding

| Type | Left-border / text color | Auto-dismiss |
|---|---|---|
| `error` (default `showToast()`) | `border-l-red-500`, `text-red-800`/`dark:text-red-200` | 5000ms |
| `warning` (`showWarningToast()`) | `border-l-amber-500`, `text-amber-800`/`dark:text-amber-200` | 8000ms |
| `success` (`showSuccessToast()`) | `border-l-emerald-500`, `text-emerald-800`/`dark:text-emerald-200` | 5000ms |
| `sticky-success` (`showStickyToast()`) | same as success | **none** - user-dismiss only, for run outcomes worth referencing (e.g. Run Rules summary); uses a distinct type key so it never groups with a plain success toast and inherits its short timer |
| `undo` | `border-l-blue-500`, `text-blue-800`/`dark:text-blue-200` | not confirmed this pass - `TODO: confirm` |
| `auth-error` | same as `error` | not confirmed - `TODO: confirm` |

All variants: solid `bg-white dark:bg-gray-800` (never glass/blur - toasts are transient chrome, not a content surface), `rounded-lg px-4 py-3 text-sm shadow-lg`, `4px` left border for the color code (matches the server-rendered `ErrorToast` fallback component exactly, which uses the identical class string - see below). Each has a dismiss (`×`) button, `aria-label` from the i18n blob (`i18n.dismiss_aria`).

### Two rendering paths, one visual contract

1. **Client-side (normal path):** `window.showToast/showSuccessToast/showWarningToast/showStickyToast` build the DOM element directly in JS (`createToastElement`), used for HTMX response errors, SSE event notifications (`sse.js`'s `toastEvents` map), and undo-flow feedback.
2. **Server-rendered fallback (`ErrorToast` component):** used **only** as raw HTML in an HTMX error-response body - the client's `htmx:responseError` handler **text-extracts** the message from this markup rather than executing it, specifically because "no inline `<script>` or interactive controls ... they never execute when the body is text-extracted" (error_toast.templ:6-10). The two paths are kept color/style-identical by hand (`colorClasses()` in layout.templ mirrors `ErrorToast`'s `templ.KV` class list) - **this is a duplication to watch**: a color/spacing change to one must be mirrored in the other, or the two toast "renderers" will visually diverge. Worth flagging as debt (or a target for consolidation) in a future pass; not yet unified in source.

### Error-message extraction (defensive, worth porting)

The `htmx:responseError` handler prefers a JSON `{"error": "..."}` envelope body, falls back to HTML-tag-stripped plain text (stripping `<script>`/`<style>` blocks **entirely**, not just their tags, before the generic tag-strip), then HTML-entity-decodes via a detached `<textarea>` (so escaped apostrophes/ampersands render correctly without risking script execution), and only surfaces the result if it's non-empty and under 500 characters - otherwise falls back to a generic templated "Request failed: %s" message. 403 responses are suppressed entirely (treated as stale admin-only fragments on a non-admin page, logged to console instead of toasted).

---

## 13. Other Interaction Affordances

### Modal open/close behavior

Confirmed shared contract across `ConfirmModal` (§5) and `CheatSheetModal` (§10): `role="dialog"`/`role="alertdialog"` + `aria-modal="true"` + `aria-labelledby`(/`aria-describedby`), a full Tab/Shift-Tab focus trap computed live off a `FOCUSABLE` selector query (buttons/links/inputs/selects/textareas not disabled, plus explicit `tabindex`, filtered to visible elements only), focus moved to a sensible default on open (close button, or first focusable) and **restored to the original opener element** on close (`_opener` captured on open). Instant `.hidden` class toggle for open/close - no CSS transition on the modal itself, only the backdrop's `transition-opacity` (which reduced-motion rules suppress automatically). Backdrop click and `Esc` both close. **Known deviation:** the merge modal's outer wrapper does not follow the `.sw-dash-card`/glass surface convention (§4) - flagged there, not repeated here.

### Lazy-load / skeleton loading states

`web/components/skeleton.templ` provides three shimmer placeholders - `SkeletonCard`, `SkeletonRow`, `SkeletonText(lines)` (last line rendered shorter "for a natural look") - plus a `Spinner` component for inline loading. All skeleton elements carry `aria-hidden="true"` (they are pure visual placeholders, not content). `Spinner` is the one component with an explicit dual-render reduced-motion strategy: an animated SVG spinner (`.sw-spinner-animated`) is CSS-hidden and a static icon (`.sw-spinner-static`) shown instead when reduced motion is active - most other animated elements in this codebase rely on the blanket `prefers-reduced-motion`/`data-motion="on"` CSS override (§1, §2) to zero out `transition-duration`, but a spinning-forever animation can't just be slowed to `0.01ms` (that would freeze it mid-rotation looking broken) - it needs a genuinely different, static visual. **Port this distinction**: reduced-motion handling is not one-size-fits-all; continuous/looping animations need a static alternate rendering, not just a duration override.

### Empty / error states

Convention confirmed from `en.json` + `artists_table.templ`: every list/report screen defines its own `{screen}.empty_state` copy key (e.g. `artists.empty_state`: "No artists found. Run a library scan to discover artists." - actionable, not just "Nothing here"), rendered as a centered, muted-text row/cell (`text-center text-sm text-gray-500 dark:text-gray-400`, `px-4 py-8`) in place of the list body. Some screens distinguish a **true-empty** state from a **filtered-to-empty** state with separate copy keys (e.g. `artist_duplicates.empty_state` "No suspected duplicates detected" vs. `artist_duplicates.empty_state_dismissed` "All suspected duplicates have been ignored") - port this distinction; a generic "No results" is less useful than telling the user *why* the list is empty and what, if anything, to do about it.

### Minimal-JS + HTMX approach (confirmed, not just asserted)

Every pattern surveyed in §9-§12 backs this up structurally: server-rendered HTML fragments swapped via HTMX (`hx-get`/`hx-post`/`hx-trigger`/`hx-swap`) for all data-driven updates (sidebar count badges, filter results, context-menu actions); hand-written JS is reserved for **interaction/state** concerns that HTMX doesn't cover - keyboard dispatch, focus management, overlay open/close, toast queuing/grouping, roving-focus restoration across swaps. No SPA framework, no client-side router, no virtual DOM. The keyboard registry (§10) explicitly rebuilds itself on `htmx:afterSwap`/`htmx:load` rather than assuming a persistent DOM - a structural acknowledgment that content is server-swapped, not client-rendered.

### Focus management across HTMX swaps (the hardest-won pattern here)

`keyboard.js`'s roving-focus-restoration logic (§10) is worth calling out on its own as a reusable technique: because `#artist-content` (or an equivalent list container) is swapped with `hx-swap="outerHTML"`, the DOM node holding focus is **destroyed and replaced** on every sort/filter/page change. The pattern: capture the focused item's **stable logical key** (`data-sw-roving-key`, not a DOM reference) on `htmx:beforeSwap`, then after the swap completes, re-locate the item with that same key in the new DOM and re-focus it (falling back to a clamped index if the exact item is gone, e.g. it was deleted). This is the general solution to "HTMX replaced the DOM out from under my focused element" and should be ported as a pattern, not reimplemented ad hoc per screen.

---

## 14. Future Directions (maintainer-stated, not yet built)

These are direction, not current state - the design-review gate should nudge new UX toward them so retrofitting is cheaper later.

- **Full internationalization.** Current: `en.json` with `fr`/`ja` falling back to `en`. Target: full i18n coverage. Design implications the gate enforces today (§6 + design-review check 7): NO hardcoded user-facing strings (all via i18n keys), and layouts resilient to translation-length expansion (German/Japanese run longer - no fixed-width text containers that clip). Forward-looking: prefer CSS **logical properties** (`margin-inline`, `padding-block`, `inset-inline-start`) over physical `left`/`right` so a future **RTL** locale mirrors without markup churn. RTL is a note, not a current requirement.
- **Flexible theming.** The one-token-per-role architecture (§3, the `--sw-*` layer) is the intended theming seam. Target: user-selectable themes beyond dark/light. Design implications the gate enforces (checks 1 + 8): all color routes through tokens, never hardcoded hex/oklch/rgb; avoid values that bake in the current palette (hardcoded shadows/tints). **Prerequisite debt:** collapse the duplicate `--sw-*` (canonical) and `--swd-*` (next-scoped alias) token layers into one - two seams make a clean theme swap ambiguous.
