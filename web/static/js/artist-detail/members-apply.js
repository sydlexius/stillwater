// Artist detail: apply members from a provider result (M55 #1336, 4A).
// Extracted verbatim from the saveMembers script in artist_field.templ. Posts a
// provider member payload, swaps the members section by id, and closes the field
// provider modal. Re-uses the #1232 safety pattern: the Apply button is disabled
// for the duration of the in-flight POST so a rapid second click cannot queue a
// duplicate, and every settled outcome re-enables it in finally().
//
// DOM contract: a button with [data-apply-members] carrying
//   data-members      -- JSON member payload (request body)
//   data-post-url     -- root-relative endpoint (base path prepended here)
//   data-target       -- CSS selector of the members section to swap (outerHTML)
//   data-save-error / data-network-error -- localized failure prefixes
// The success branch sends HX-Request:true so the server returns the
// re-rendered MembersSection HTML fragment (error path returns the ErrorToast
// fragment OR a JSON {error} envelope; the parser tolerates either).
//
// Export surface: window.swArtistMembersApply doubles as the load-once guard.
(function () {
  'use strict';
  if (window.swArtistMembersApply) return;

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest && ev.target.closest('[data-apply-members]');
    if (!btn) return;
    ev.preventDefault();
    if (btn.disabled) return;
    btn.disabled = true;

    var members = btn.getAttribute('data-members');
    var url = btn.getAttribute('data-post-url');
    var targetSelector = btn.getAttribute('data-target');
    var saveErrorPrefix = btn.getAttribute('data-save-error') || 'Failed to save members';
    var networkError = btn.getAttribute('data-network-error') || 'Network error saving members';
    // Canonical CSRF reader from preferences.js (loaded by Layout before any
    // click reaches here); fall back to empty so a missing helper degrades to a
    // server-side CSRF rejection rather than a JS error.
    var csrf = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    var bp = (document.querySelector('meta[name="htmx-base-path"]') || { content: '' }).content;
    var notify = function (msg) {
      if (typeof window.showToast === 'function') { window.showToast(msg); } else { alert(msg); }
    };

    fetch(bp + url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'HX-Request': 'true', 'X-CSRF-Token': csrf },
      body: members,
      credentials: 'same-origin'
    }).then(function (r) {
      if (r.ok) {
        return r.text().then(function (html) {
          var target = document.querySelector(targetSelector);
          if (target) {
            target.outerHTML = html;
            var newTarget = document.querySelector(targetSelector);
            if (newTarget && typeof htmx !== 'undefined') { htmx.process(newTarget); }
          }
          if (typeof hideFieldProviderModal === 'function') { hideFieldProviderModal(); }
        });
      }
      return r.text().then(function (body) {
        var msg = saveErrorPrefix + ' (' + r.status + ')';
        var trimmed = (body || '').trim();
        var stripped = '';
        var isJSON = false;
        if (trimmed.length > 0 && trimmed.charAt(0) === '{') {
          try {
            var parsed = JSON.parse(trimmed);
            if (parsed && typeof parsed.error === 'string') { stripped = parsed.error.trim(); isJSON = true; }
          } catch (e) { /* not JSON; fall through to HTML strip */ }
        }
        if (!isJSON && trimmed.length > 0) {
          stripped = trimmed.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
        }
        if (stripped.length > 0 && stripped.length < 500) { msg = saveErrorPrefix + ': ' + stripped; }
        notify(msg);
      });
    }).catch(function () {
      notify(networkError);
    }).finally(function () {
      btn.disabled = false;
    });
  });

  window.swArtistMembersApply = true;
})();
