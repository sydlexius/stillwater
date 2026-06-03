// Settings: extracted from the inline romanizationFallbackScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS body is
// verbatim except for this load-once guard. The original was already wrapped in
// its own IIFE and already assigned window.swToggleRomanizationFallback, so no
// extra window re-export was added. No CSRF change: this module does not fetch
// directly -- it persists through window.swPreferences.set(), which owns CSRF.
//
// DOM contract: bound via onclick="swToggleRomanizationFallback(this)" on a
//   role=switch button carrying data-toast-saved / data-toast-failed i18n
//   strings; toggles aria-checked and the bg-/translate- Tailwind classes.
// Network: none directly; delegates to window.swPreferences.set(
//   'metadata_name_romanization_fallback', "true"|"false"). Success/failure
//   feedback via the global showSuccessToast()/showToast().
//
// Export surface: window.swRomanizationFallback doubles as the load-once guard.
// window.swToggleRomanizationFallback is assigned inside the body (verbatim)
// because the toggle calls it by bare name.
(function () {
  'use strict';

  if (window.swRomanizationFallback) return;

  (function () {
    var romanizationSeq = 0;
    // Initialized lazily on the first click from the clicked control.
    // The script is injected ahead of the toggle in the DOM, so reading
    // the button at parse time would always miss it.
    var lastConfirmedRomanization = null;
    var romanizationPending = false;

    function applyToggleState(btn, knob, isOn) {
      btn.setAttribute('aria-checked', String(isOn));
      if (isOn) {
        btn.classList.remove('bg-gray-200', 'dark:bg-gray-600');
        btn.classList.add('bg-blue-600');
        knob.classList.remove('translate-x-0');
        knob.classList.add('translate-x-5');
      } else {
        btn.classList.remove('bg-blue-600');
        btn.classList.add('bg-gray-200', 'dark:bg-gray-600');
        knob.classList.remove('translate-x-5');
        knob.classList.add('translate-x-0');
      }
    }

    window.swToggleRomanizationFallback = function (btn) {
      // Serialize: ignore clicks while a save is in flight.
      if (romanizationPending) return;
      var isOn = btn.getAttribute('aria-checked') === 'true';
      if (lastConfirmedRomanization === null) {
        lastConfirmedRomanization = String(isOn);
      }
      var newVal = !isOn;
      var valStr = String(newVal);
      var knob = btn.querySelector('span');
      var seq = ++romanizationSeq;
      romanizationPending = true;
      var toastSaved = btn.getAttribute('data-toast-saved') || 'Romanization fallback preference saved';
      var toastFailed = btn.getAttribute('data-toast-failed') || 'Failed to save romanization fallback preference';

      function rollback() {
        applyToggleState(btn, knob, lastConfirmedRomanization === 'true');
      }

      // Optimistic UI update.
      applyToggleState(btn, knob, newVal);

      if (window.swPreferences) {
        window.swPreferences.set('metadata_name_romanization_fallback', valStr).then(function (saved) {
          if (saved === 'true' || saved === 'false') {
            lastConfirmedRomanization = saved;
          }
          if (seq !== romanizationSeq) return;
          if (saved === valStr) {
            if (typeof window.showSuccessToast === 'function') {
              showSuccessToast(toastSaved);
            }
          } else {
            rollback();
            if (typeof window.showToast === 'function') {
              showToast(toastFailed);
            }
          }
        }).catch(function () {
          if (seq !== romanizationSeq) return;
          rollback();
          if (typeof window.showToast === 'function') {
            showToast(toastFailed);
          }
        }).finally(function () {
          if (seq === romanizationSeq) romanizationPending = false;
        });
      } else {
        rollback();
        romanizationPending = false;
        if (typeof window.showToast === 'function') {
          showToast(toastFailed);
        }
      }
    };
  }());

  window.swRomanizationFallback = { loaded: true };
})();
