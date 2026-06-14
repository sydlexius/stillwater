// Shared client-side utility helpers.
// Loaded once per page via layout.templ (LayoutGlobalChrome) and
// onboarding.templ so all inline scripts can call the globals below
// without each defining their own copy.
//
// Public API:
//   window.escapeHTML(str) -- escape HTML special characters in a string
(function () {
  'use strict';

  // escapeHTML converts characters that are special in HTML into their entity
  // equivalents, preventing XSS when interpolating untrusted text into markup.
  // Covers the five characters required for safe HTML-attribute and text-node
  // contexts: & < > " '
  window.escapeHTML = function (str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  };
}());
