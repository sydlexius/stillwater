// session.js -- HTMX 401 session timeout handler.
//
// Listens for failed HTMX responses and redirects to the login page when the
// server returns 401 (session expired or missing). The current page URL is
// passed as a return parameter so the user lands back where they were after
// re-authenticating.
//
// The login page is served at GET / (the root) when the user is not
// authenticated. There is no dedicated /login route.
//
// This script is only loaded by Layout (authenticated pages). The login page
// uses its own template and does not include this script, so there is no risk
// of redirect loops when redirecting to the root.

(function () {
  "use strict";

  // Read the base path from the meta tag so sub-path deployments work
  // (e.g. when the app is served at /stillwater instead of /).
  var bpEl = document.querySelector('meta[name="htmx-base-path"]');
  var bp = bpEl ? bpEl.content : '';
  var loginPath = bp + '/';

  document.body.addEventListener("htmx:responseError", function (evt) {
    var xhr = evt.detail.xhr;
    if (!xhr || xhr.status !== 401) {
      return;
    }

    // Clear cached preferences so the next user does not see stale settings.
    if (window.swPreferences) { window.swPreferences.clearCache(); }

    // Strip the base path prefix so the return URL is basePath-relative.
    // The server prepends basePath when building the redirect, so sending
    // the full pathname would cause double-prefixing (e.g. /sw/sw/artists).
    var relPath = window.location.pathname;
    if (bp && (relPath === bp || relPath.indexOf(bp + '/') === 0)) {
      relPath = relPath.substring(bp.length) || '/';
    }
    var returnURL = relPath + window.location.search + window.location.hash;
    window.location.href = loginPath + "?return=" + encodeURIComponent(returnURL);
  });
})();
