// a11y-fixtures.js - Minimal HTML fixtures for jsdom-based axe-core structural
// a11y tests. Each fixture is a representative excerpt of its component's live
// output (derived from the corresponding .templ source) with enough markup to
// exercise the target rules: button-name, label, aria-*, role validation.
//
// Intentional scope: jsdom has no CSS cascade so color-contrast rules are
// suppressed here and handled by the Playwright smoke tier (tests/a11y/).
//
// Fixtures are kept minimal but faithful: they preserve the element structure,
// ARIA attributes, and role assignments from the templates.

// ---------------------------------------------------------------------------
// Bulk-action bar (web/templates/next/bulk.templ -- nextBulkStrip, non-contextual)
// ---------------------------------------------------------------------------
export const bulkBarFixture = `<!doctype html>
<html lang="en">
<body>
  <div id="artist-content" role="region" aria-label="Artist list">
    <div
      id="bulk-action-bar"
      class="sw-next-bulk-strip"
      role="toolbar"
      aria-label="Bulk actions"
      data-contextual="false"
      data-selection-active="false"
    >
      <label class="inline-flex items-center gap-2 text-sm">
        <input
          id="bulk-select-all"
          type="checkbox"
          class="h-4 w-4"
          aria-controls="artist-content"
          aria-label="Select all artists"
        />
        <span id="bulk-select-all-label">Select all</span>
      </label>

      <span id="bulk-selected-count" class="text-sm" aria-live="polite">
        None selected
      </span>

      <button
        id="bulk-show-selected"
        type="button"
        class="hidden"
        aria-label="Show selected artists"
      >
        Show selected
      </button>

      <button
        id="bulk-select-all-matching"
        type="button"
        class="hidden"
        aria-label="Select all matching artists"
      >
        Select all matching
      </button>

      <button
        id="bulk-deselect-all"
        type="button"
        class="hidden"
        aria-label="Deselect all"
      >
        Deselect all
      </button>

      <select
        id="bulk-action-select"
        disabled
        aria-label="Bulk action"
      >
        <option value="">Choose action</option>
        <option value="run_rules">Run rules</option>
        <option value="scan">Scan</option>
        <option value="fetch_images">Fetch images</option>
        <option value="lock">Lock</option>
        <option value="unlock">Unlock</option>
      </select>

      <button
        id="bulk-action-apply"
        type="button"
        disabled
        aria-label="Apply bulk action"
      >
        Apply
      </button>
    </div>
  </div>
</body>
</html>`;

// ---------------------------------------------------------------------------
// Artwork modal (web/templates/next/artist_artwork_modal.templ -- ArtworkModal)
// ---------------------------------------------------------------------------
export const artworkModalFixture = `<!doctype html>
<html lang="en">
<body>
  <div
    id="artwork-modal"
    class="hidden"
    role="dialog"
    aria-modal="true"
    aria-labelledby="artwork-modal-title"
    data-artist-id="artist-fixture-1"
    tabindex="-1"
  >
    <div class="modal-surface">
      <div class="modal-header">
        <div>
          <h2 id="artwork-modal-title" class="text-lg font-semibold">Manage artwork</h2>
          <p class="text-xs">Select and apply artwork for this artist.</p>
        </div>
        <button
          type="button"
          class="modal-close"
          data-sw-artwork-close
          aria-label="Close"
        >
          <span aria-hidden="true">&times;</span>
        </button>
      </div>

      <div role="group" aria-label="Artwork kind">
        <button
          type="button"
          class="sw-artwork-kind-tab"
          data-sw-artwork-kind-tab
          data-artwork-kind="primary"
          aria-pressed="true"
        >
          Primary
        </button>
        <button
          type="button"
          class="sw-artwork-kind-tab"
          data-sw-artwork-kind-tab
          data-artwork-kind="logo"
          aria-pressed="false"
        >
          Logo
        </button>
        <button
          type="button"
          class="sw-artwork-kind-tab"
          data-sw-artwork-kind-tab
          data-artwork-kind="banner"
          aria-pressed="false"
        >
          Banner
        </button>
        <button
          type="button"
          class="sw-artwork-kind-tab"
          data-sw-artwork-kind-tab
          data-artwork-kind="backdrops"
          aria-pressed="false"
        >
          Backdrops
        </button>
      </div>

      <div
        id="artwork-gate-banner"
        class="hidden"
        role="alert"
      >
        <span>Artwork update paused</span>
        <span id="artwork-gate-reason"></span>
      </div>

      <div id="artwork-revert-row" class="hidden">
        <button
          id="artwork-revert-btn"
          type="button"
          aria-label="Revert to original artwork"
        >
          Revert to original
        </button>
      </div>

      <div id="artwork-modal-body" aria-live="polite">
        <p>Loading...</p>
      </div>
    </div>
  </div>
</body>
</html>`;

// ---------------------------------------------------------------------------
// Dashboard stat cards (web/templates/next/dashboard.templ -- DashboardPageNext)
// ---------------------------------------------------------------------------
export const dashboardCardsFixture = `<!doctype html>
<html lang="en">
<body>
  <div class="sw-next-dashboard">
    <h1 class="sr-only">Dashboard</h1>

    <div class="sw-next-header-strip grid grid-cols-2 gap-2">
      <!-- Library health bubble -->
      <div class="sw-stat-bubble">
        <svg width="28" height="28" aria-hidden="true">
          <circle cx="14" cy="14" r="11" fill="none" stroke-width="3"></circle>
          <circle cx="14" cy="14" r="11" fill="none" stroke-width="3"
            stroke-dasharray="62.2" stroke-dashoffset="12.44"
            class="text-green-500"></circle>
        </svg>
        <div>
          <span class="sw-stat-label">Library health</span>
          <span class="sw-stat-val">80%</span>
        </div>
      </div>

      <!-- Total artists bubble (link for navigation) -->
      <a
        class="sw-stat-bubble"
        href="/next/artists"
        aria-label="1234 total artists"
      >
        <span class="sw-stat-label">Artists</span>
        <span class="sw-stat-val">1234</span>
      </a>

      <!-- Last-evaluated bubble (button) -->
      <button
        type="button"
        id="last-evaluated-btn"
        class="sw-stat-bubble text-left"
        aria-label="Last evaluated: 2 hours ago"
      >
        <span class="sw-stat-label">Last evaluated</span>
        <span id="last-evaluated-value" class="sw-stat-val">2h ago</span>
      </button>

      <!-- Rule violations bubble -->
      <div class="sw-stat-bubble">
        <span class="sw-stat-label">Auto-fixable</span>
        <span class="sw-stat-val">12</span>
      </div>

      <!-- Needs review bubble -->
      <div class="sw-stat-bubble">
        <span class="sw-stat-label">Needs review</span>
        <span class="sw-stat-val">3</span>
      </div>
    </div>
  </div>
</body>
</html>`;

// ---------------------------------------------------------------------------
// Prefs drawer (web/templates/next/prefs_drawer.templ)
// ---------------------------------------------------------------------------
export const prefsDrawerFixture = `<!doctype html>
<html lang="en">
<body>
  <!-- Scrim (visual only) -->
  <div class="sw-prefs-scrim" aria-hidden="true"></div>

  <div
    class="sw-prefs-drawer"
    role="dialog"
    aria-modal="true"
    aria-labelledby="sw-prefs-drawer-title"
    aria-hidden="true"
  >
    <div class="sw-prefs-drawer-header">
      <div class="sw-prefs-drawer-header-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="5"/></svg>
      </div>
      <h2 id="sw-prefs-drawer-title" class="sw-prefs-drawer-title">Preferences</h2>
      <button
        type="button"
        class="sw-prefs-drawer-close"
        aria-label="Close preferences"
      >
        <span aria-hidden="true">&times;</span>
      </button>
    </div>

    <div class="sw-prefs-drawer-search">
      <input
        type="search"
        class="sw-prefs-drawer-search-input"
        placeholder="Search preferences"
        aria-label="Search preferences"
      />
    </div>

    <div class="sw-prefs-drawer-body">
      <p
        id="sw-prefs-search-empty"
        class="sw-prefs-search-empty"
        role="status"
        aria-live="polite"
        hidden
      >
        No preferences match your search.
      </p>

      <!-- Theme row (tile group) -->
      <div role="radiogroup" aria-labelledby="pref-d-theme-label">
        <span id="pref-d-theme-label">Theme</span>
        <button type="button" role="radio" aria-checked="true" aria-label="System">System</button>
        <button type="button" role="radio" aria-checked="false" aria-label="Light">Light</button>
        <button type="button" role="radio" aria-checked="false" aria-label="Dark">Dark</button>
      </div>

      <!-- Reduced motion row (segmented) -->
      <div role="radiogroup" aria-labelledby="pref-d-reduced-motion-label">
        <span id="pref-d-reduced-motion-label">Reduced motion</span>
        <button type="button" role="radio" aria-checked="true" aria-label="Off">Off</button>
        <button type="button" role="radio" aria-checked="false" aria-label="On">On</button>
      </div>

      <!-- Keyboard hints toggle -->
      <div>
        <button
          type="button"
          role="switch"
          aria-checked="false"
          aria-label="Show keyboard hints"
        >
          Show keyboard hints
        </button>
      </div>
    </div>

    <div class="sw-prefs-drawer-footer">
      <button
        type="button"
        class="sw-prefs-reset-btn"
        aria-label="Reset all preferences to defaults"
      >
        Reset all
      </button>
    </div>
  </div>
</body>
</html>`;
