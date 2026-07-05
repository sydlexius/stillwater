// Show/hide toggle for masked credential inputs (SecretInput component,
// web/components/secret_input.templ, #2102).
//
// Delegated on document rather than bound per-instance: the provider key
// form and the connection add/edit forms are re-rendered via HTMX innerHTML
// swaps, so a fresh SecretInput can appear at any time without a JS init
// call. No public API is required for normal use; apply() is exported for
// tests.
(function () {
  'use strict';

  // apply flips the input between masked (password) and revealed (text),
  // syncing aria-pressed, aria-label, and which eye/eye-slash icon shows.
  function apply(wrapper, revealed) {
    var input = wrapper.querySelector('input');
    var btn = wrapper.querySelector('[data-sw-secret-toggle]');
    if (!input || !btn) return;

    input.type = revealed ? 'text' : 'password';
    btn.setAttribute('aria-pressed', String(revealed));

    var label = revealed
      ? btn.getAttribute('data-label-hide')
      : btn.getAttribute('data-label-show');
    if (label) btn.setAttribute('aria-label', label);

    var showIcon = wrapper.querySelector('[data-sw-secret-icon="show"]');
    var hideIcon = wrapper.querySelector('[data-sw-secret-icon="hide"]');
    if (showIcon) showIcon.classList.toggle('hidden', revealed);
    if (hideIcon) hideIcon.classList.toggle('hidden', !revealed);
  }

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest('[data-sw-secret-toggle]');
    if (!btn) return;
    var wrapper = btn.closest('[data-sw-secret-input]');
    if (!wrapper) return;
    var input = wrapper.querySelector('input');
    if (!input) return;
    apply(wrapper, input.type !== 'text');
  });

  window.swSecretToggle = { apply: apply };
})();
