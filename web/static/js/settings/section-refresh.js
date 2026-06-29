// Settings: targeted section refresh after a save/activate mutation (M55 #1339
// items 1+6).
//
// WHY THIS EXISTS. The reused stable settings controls used to call
// window.location.reload() after a successful mutation. On the stable /settings
// page (tabbed, one short panel visible) that was tolerable. On /next/settings
// -- a single long-scroll pane behind a fixed full-viewport ambient backdrop
// image -- the full-page reload (a) discarded the scroll position so the
// viewport jumped away from where the user clicked (BUG-1), and (b) re-requested
// /api/v1/images/random-backdrop, rolling a NEW random backdrop that popped into
// existence (BUG-6). Both are the same defect: reload-on-save.
//
// THE FIX. swRefreshSettingsSection(name) updates ONLY the changed section, with
// no document reload:
//   1. fetch the CURRENT page URL (same channel as the live page, so the section
//      re-renders at the correct heading level / with the correct channel chrome
//      automatically -- no server-side fragment endpoint or ?level param needed).
//   2. Parse the response with DOMParser. The resulting document is INERT: its
//      <img> tags (including the ambient backdrop) are NOT fetched by the
//      browser, so no new random backdrop is rolled.
//   3. Extract the single [data-settings-fragment="<name>"] node from the parsed
//      document and replaceWith() the live one. The wrappers are INNER
//      containers (below the stable tab-`hidden` toggle and below the card
//      heading), so the swapped node never carries tab-hidden state and never
//      re-renders a heading -- stable parity safe.
//   4. Re-run htmx.process() on the new node so its hx-* controls re-bind, and
//      dispatch htmx:afterSwap so sortable-init (and any other afterSwap
//      consumer) re-attaches to freshly rendered nodes.
//
// The user's scroll position is untouched (we never reload, never scroll) and
// the backdrop element is never replaced.
//
// Loaded on BOTH the stable settings page and /next/settings so the shared
// controls behave identically.
//
// Export surface: window.swRefreshSettingsSection (the callable) and
// window.swSectionRefresh (the load-once guard).
(function () {
  'use strict';

  if (window.swSectionRefresh) return;

  // Per-name request token: a section can be refreshed several times in quick
  // succession (rapid saves). Each refresh stamps the next token for its name;
  // a slower OLDER fetch that resolves after a NEWER one must NOT clobber the
  // newer result (which would revert to stale HTML), so it bails when its token
  // is no longer the latest.
  var latestRefreshByName = {};

  // refreshSection re-fetches the current page and swaps in the fresh copy of
  // the one [data-settings-fragment="name"] section. Returns a Promise so tests
  // (and callers that want to chain) can await completion; callers in templates
  // fire-and-forget.
  function refreshSection(name) {
    var live = document.querySelector('[data-settings-fragment="' + name + '"]');
    if (!live) {
      // Loud failure: a caller asked to refresh a section whose wrapper is not
      // on the page. Never silently no-op.
      console.error('swRefreshSettingsSection: no live [data-settings-fragment="' + name + '"] on the page');
      window.location.reload();
      return Promise.resolve(false);
    }

    latestRefreshByName[name] = (latestRefreshByName[name] || 0) + 1;
    var refreshID = latestRefreshByName[name];

    return fetch(window.location.href, {
      credentials: 'same-origin',
      headers: { 'X-Requested-With': 'fetch' }
    }).then(function (resp) {
      if (!resp.ok) {
        throw new Error('refresh fetch failed: HTTP ' + resp.status);
      }
      return resp.text();
    }).then(function (html) {
      // A newer refresh for this section superseded us while in flight; drop
      // this stale response before doing any parse/swap work.
      if (latestRefreshByName[name] !== refreshID) {
        return false;
      }
      // Inert parse: images in this document are NOT requested by the browser,
      // so the ambient backdrop is never re-rolled.
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var fresh = doc.querySelector('[data-settings-fragment="' + name + '"]');
      if (!fresh) {
        console.error('swRefreshSettingsSection: [data-settings-fragment="' + name + '"] missing from the refreshed page');
        window.location.reload();
        return false;
      }
      // Re-check immediately before the swap: a newer refresh may have landed
      // during DOMParser work, and the swap is the destructive step.
      if (latestRefreshByName[name] !== refreshID) {
        return false;
      }
      // Adopt the node into the live document, then swap it in for the stale one.
      var imported = document.importNode(fresh, true);
      live.replaceWith(imported);

      // Re-bind htmx controls on the new subtree (Test / Delete / revoke buttons
      // carry hx-* attributes that htmx must re-scan after a manual swap).
      if (window.htmx && typeof window.htmx.process === 'function') {
        window.htmx.process(imported);
      } else {
        console.error('swRefreshSettingsSection: htmx unavailable; hx-* controls in the refreshed section will not re-bind');
      }
      // Mirror an htmx swap so afterSwap consumers (sortable-init re-attaches
      // Sortable to freshly rendered [data-sortable-field] rows) re-initialize.
      document.body.dispatchEvent(new CustomEvent('htmx:afterSwap', {
        bubbles: true,
        detail: { elt: imported, target: imported }
      }));
      return true;
    }).catch(function (err) {
      console.error('swRefreshSettingsSection: ' + name + ' refresh failed', err);
      window.location.reload();
      return false;
    });
  }

  window.swRefreshSettingsSection = refreshSection;
  window.swSectionRefresh = { refresh: refreshSection };
})();
