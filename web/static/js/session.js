// session.js -- HTMX 401 session timeout handler.
//
// Listens for failed HTMX responses and redirects to the login page when the
// server returns 401 (session expired or missing). The current page URL is
// passed as a return parameter so the user lands back where they were after
// re-authenticating.
//
// This script must be loaded on every authenticated page (included in the
// main layout). It intentionally does nothing on the login page itself to
// avoid redirect loops.

(function () {
  "use strict";

  // Read the base path from the meta tag so sub-path deployments work
  // (e.g. when the app is served at /stillwater instead of /).
  var bpEl = document.querySelector('meta[name="htmx-base-path"]');
  var bp = bpEl ? bpEl.content : '';
  var loginPath = bp + '/login';

  document.body.addEventListener("htmx:responseError", function (evt) {
    var xhr = evt.detail.xhr;
    if (!xhr || xhr.status !== 401) {
      return;
    }

    // Do not redirect if already on the login page.
    var path = window.location.pathname;
    if (path === loginPath || path === loginPath + '/') {
      return;
    }

    var returnURL = window.location.pathname + window.location.search;
    window.location.href = loginPath + "?return=" + encodeURIComponent(returnURL);
  });
})();
