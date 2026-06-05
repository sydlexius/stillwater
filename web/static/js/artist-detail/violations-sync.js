// Artist detail: violations live-sync (M55 #1336, 4A).
// Extracted from the artistDetailViolationSync script in artist_detail.templ.
// Keeps the violations surface + badge in sync with violation-state changes
// from any source: the dashboard:action-resolved event (fix/dismiss), an
// htmx:afterSettle on #refresh-panel (metadata refresh re-runs rules), and the
// sse:artist.updated SSE event.
//
// DOM contract:
//   [data-sw-violations-sync] -- mount element carrying data-artist-id.
//   #violations-content-{id} or #artist-violations-tab-{id} -- swap targets;
//     the server response includes an OOB badge swap so the pill stays in sync.
//   #refresh-panel -- metadata-refresh result container.
// Root-relative URLs only: the global htmx:configRequest hook in layout.templ
// prepends the base path, so concatenating it here would double-prefix.
//
// Export surface: window.swArtistViolationsSync doubles as the load-once guard.
(function () {
  'use strict';
  if (window.swArtistViolationsSync) return;

  function init() {
    var mount = document.querySelector('[data-sw-violations-sync]');
    if (!mount) return;
    var artistID = mount.dataset.artistId;
    if (!artistID) return;

    var tabURL = '/artists/' + artistID + '/violations/tab';
    var loadedTarget = '#violations-content-' + artistID;
    var initialTarget = '#artist-violations-tab-' + artistID;

    function refreshViolationsTab() {
      var target = document.querySelector(loadedTarget) || document.querySelector(initialTarget);
      if (!target || typeof htmx === 'undefined') return;
      htmx.ajax('GET', tabURL, { target: target, swap: 'outerHTML' });
    }

    document.body.addEventListener('dashboard:action-resolved', refreshViolationsTab);

    var refreshPanel = document.getElementById('refresh-panel');
    if (refreshPanel) {
      refreshPanel.addEventListener('htmx:afterSettle', refreshViolationsTab);
    }

    document.addEventListener('sse:artist.updated', function (evt) {
      var data = (evt && evt.detail) || {};
      if (!data.artist_id || data.artist_id === artistID) {
        refreshViolationsTab();
      }
    });

    // Stable channel only: Run Rules emits artist:show-violations-tab to flip to
    // the violations tab. next/ is single-scroll and has no #tab-violations, so
    // this branch is a no-op there (getElementById returns null).
    document.body.addEventListener('artist:show-violations-tab', function () {
      var btn = document.getElementById('tab-violations');
      if (btn) btn.click();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.swArtistViolationsSync = { init: init };
})();
