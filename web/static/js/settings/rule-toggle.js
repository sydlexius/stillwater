// Settings: extracted from the inline ruleToggleScript() (M55 #1808).
// Lift out of web/templates/settings.templ; the JS is verbatim except for this
// IIFE wrapper, the load-once guard, the window re-exports of the functions
// called by bare name from rule-card markup, and the two CSRF reads (in
// patchRule and runRule) routed through the canonical window.swCsrfToken()
// helper (preferences.js) instead of inline cookie-parse regexes.
//
// Save error-handling hardened per the #1808 acceptance criteria (consistent
// failure feedback): the internal patchRuleStrict rejects on a non-2xx/network
// failure so the enabled toggle rolls back to its prior state and the config
// panel stays open, rather than leaving the UI showing a value the server never
// persisted. The toggle also ignores re-entrant clicks while a save is in
// flight (data-inflight guard) so overlapping PUTs cannot resolve out of order.
//
// DOM contract (rule cards in settings.templ):
//   onclick="toggleRuleEnabled(this)"  -- role=switch, data-rule-id; toggles
//     aria-checked + bg-/translate- classes and enables/disables the row's
//     select[data-rule-id] and [data-run-btn].
//   onclick="runRule(this)"            -- run button, data-rule-id.
//   onsubmit="handleRuleConfigSubmit(event)" -- per-rule config form.
//   onchange="patchRule(this.dataset.ruleId, {automation_mode: this.value})"
//     -- automation-mode select (uses the window.patchRule wrapper).
//   onchange="applyResPreset(this)" / onchange="applyAspectPreset(this)"
//     -- resolution / aspect-ratio preset selects.
//   Toasts render into #error-toast-container (falls back to global showToast).
// Network (all base-path aware, csrf_token sent as X-CSRF-Token):
//   PUT  {base}/api/v1/rules/{id}            (patchRuleStrict: enabled/config/automation_mode)
//   POST {base}/api/v1/rules/{id}/run        (runRule)
//   GET  {base}/api/v1/rules/run-all/status  (pollRuleStatus, via pollAsyncStatus)
//
// Export surface: window.swRuleToggle doubles as the load-once guard. The five
// HTML-referenced handlers plus window.patchRule (a toast-wrapping shim over the
// internal rejecting patchRuleStrict) are assigned to window; patchRuleStrict/
// pollRuleStatus/finishRunBtn/showRuleToast stay internal.
(function () {
  'use strict';

  if (window.swRuleToggle) return;

  var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;

  // patchRuleStrict returns the fetch promise and rejects on a non-2xx response
  // so internal callers can roll back their optimistic UI; network errors reject
  // naturally. The caller owns failure feedback (it knows which control to
  // revert), so no toast is shown here. The inline `onchange="patchRule(...)"`
  // call site (automation-mode select) uses the window.patchRule wrapper below
  // instead, which swallows the rejection with a toast.
  function patchRuleStrict(ruleID, changes) {
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
    return fetch(bp + '/api/v1/rules/' + ruleID, {
      method: 'PUT',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken},
      body: JSON.stringify(changes),
      credentials: 'same-origin'
    }).then(function(r) {
      if (!r.ok) {
        throw new Error('Failed to update rule.');
      }
    });
  }

  function toggleRuleEnabled(btn) {
    // Serialize: ignore clicks while a save is in flight so overlapping PUTs
    // cannot resolve out of order and revert the switch to a stale state.
    if (btn.dataset.inflight === '1') return;
    var ruleID = btn.dataset.ruleId;
    var isOn = btn.getAttribute('aria-checked') === 'true';
    var newVal = !isOn;
    var knob = btn.querySelector('span');
    var row = btn.closest('.py-4');
    var sel = row ? row.querySelector('select[data-rule-id]') : null;
    var runBtn = row ? row.querySelector('[data-run-btn]') : null;

    function applyEnabledState(value) {
      btn.setAttribute('aria-checked', String(value));
      if (value) {
        btn.classList.remove('bg-gray-200', 'dark:bg-gray-600');
        btn.classList.add('bg-blue-600');
        knob.classList.remove('translate-x-0');
        knob.classList.add('translate-x-5');
        if (sel) sel.disabled = false;
        if (runBtn) runBtn.disabled = false;
      } else {
        btn.classList.remove('bg-blue-600');
        btn.classList.add('bg-gray-200', 'dark:bg-gray-600');
        knob.classList.remove('translate-x-5');
        knob.classList.add('translate-x-0');
        if (sel) sel.disabled = true;
        if (runBtn) runBtn.disabled = true;
      }
    }

    // Optimistic update, rolled back to the prior state if the save fails.
    btn.dataset.inflight = '1';
    applyEnabledState(newVal);
    patchRuleStrict(ruleID, {enabled: newVal}).catch(function() {
      applyEnabledState(isOn);
      if (typeof showToast === 'function') {
        showToast('Failed to update rule.');
      }
    }).then(function() {
      delete btn.dataset.inflight;
    });
  }

  function runRule(btn) {
    var ruleID = btn.dataset.ruleId;
    var origText = btn.textContent;
    btn.dataset.running = 'true';
    btn.disabled = true;
    btn.textContent = 'Running...';
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
    fetch(bp + '/api/v1/rules/' + ruleID + '/run', {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken},
      credentials: 'same-origin'
    }).then(function(r) {
      if (r.status === 409) { showRuleToast('A rule evaluation is already running.', true); return null; }
      if (!r.ok) throw new Error('status ' + r.status);
      return r.json();
    }).then(function(data) {
      if (!data) { finishRunBtn(btn, origText); return; }
      // Server accepted (202) -- poll for completion.
      pollRuleStatus(btn, origText);
    }).catch(function() {
      showRuleToast('Failed to start rule evaluation.', true);
      finishRunBtn(btn, origText);
    });
  }

  function pollRuleStatus(btn, origText) {
    pollAsyncStatus(bp + '/api/v1/rules/run-all/status', {
      onData: function(s) {
        if (s.status === 'running') return false;
        if (s.status === 'completed') {
          var msg;
          if (s.violations_found === 0) {
            msg = 'Evaluated ' + s.artists_processed + ' artists: no violations found';
          } else if (s.violations_remaining === 0 && s.violations_auto_fixed > 0) {
            msg = 'Evaluated ' + s.artists_processed + ' artists: ' + s.violations_found + ' violations, all auto-fixed';
          } else if (s.violations_auto_fixed > 0) {
            msg = 'Evaluated ' + s.artists_processed + ' artists: ' + s.violations_found + ' violations (' + s.violations_auto_fixed + ' auto-fixed, ' + s.violations_remaining + ' remaining)';
          } else {
            msg = 'Evaluated ' + s.artists_processed + ' artists: ' + s.violations_found + ' violations (' + s.violations_remaining + ' remaining)';
          }
          showRuleToast(msg, false);
        } else if (s.status === 'failed') {
          var errMsg = s.error ? 'Rule evaluation failed: ' + s.error : 'Rule evaluation failed.';
          showRuleToast(errMsg, true);
        } else if (s.status === 'idle') {
          showRuleToast('Rule evaluation state lost (server may have restarted).', true);
        }
        finishRunBtn(btn, origText);
        return true;
      },
      onHTTPError: function(status) {
        showRuleToast('Status check failed (HTTP ' + status + ').', true);
        finishRunBtn(btn, origText);
      },
      onNetworkError: function() {
        showRuleToast('Lost connection while running rule.', true);
        finishRunBtn(btn, origText);
      }
    }, {maxAttempts: 0});
  }

  function finishRunBtn(btn, origText) {
    delete btn.dataset.running;
    btn.textContent = origText;
    var row = btn.closest('.py-4');
    var toggle = row ? row.querySelector('[role="switch"]') : null;
    var isEnabled = toggle ? toggle.getAttribute('aria-checked') === 'true' : true;
    btn.disabled = !isEnabled;
  }

  // showRuleToast displays a brief toast for rule run results.
  // Uses green for success, red for errors.
  function showRuleToast(msg, isError) {
    var container = document.getElementById('error-toast-container');
    if (!container) { if (typeof showToast === 'function') showToast(msg); return; }
    var toast = document.createElement('div');
    var colors = isError
      ? 'bg-red-50 dark:bg-red-900/50 text-red-800 dark:text-red-200 border-red-200 dark:border-red-800'
      : 'bg-green-50 dark:bg-green-900/50 text-green-800 dark:text-green-200 border-green-200 dark:border-green-800';
    toast.className = 'flex items-center gap-3 rounded-lg px-4 py-3 text-sm shadow-lg border transition-opacity duration-300 ' + colors;
    var span = document.createElement('span');
    span.textContent = msg;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute('aria-label', 'Dismiss notification');
    btn.className = 'ml-2 font-bold opacity-70 hover:opacity-100';
    btn.textContent = '\u00d7';
    btn.onclick = function() { toast.remove(); };
    toast.appendChild(span);
    toast.appendChild(btn);
    container.appendChild(toast);
    setTimeout(function() { toast.style.opacity = '0'; setTimeout(function() { toast.remove(); }, 300); }, 5000);
  }

  function handleRuleConfigSubmit(event) {
    event.preventDefault();
    var form = event.target;
    var ruleID = form.dataset.ruleId;
    var cfg = {};
    var intFields = {'min_width':1, 'min_height':1, 'min_length':1, 'trim_margin':1};
    ['min_width', 'min_height', 'aspect_ratio', 'tolerance', 'min_length', 'threshold_percent', 'trim_margin', 'coverage_threshold'].forEach(function(f) {
      var el = form.elements[f];
      if (el && el.value !== '') cfg[f] = intFields[f] ? parseInt(el.value, 10) : parseFloat(el.value);
    });
    // Text-valued config fields (e.g. the discography release-type filter)
    // are forwarded as-is rather than parsed as numbers.
    ['release_types'].forEach(function(f) {
      var el = form.elements[f];
      if (!el) return;
      var value = el.value.trim();
      if (value !== '') cfg[f] = value;
    });
    var sev = form.elements['severity'];
    if (sev) cfg['severity'] = sev.value;
    var panel = document.getElementById('rule-cfg-' + ruleID);
    // Only collapse the config panel once the save succeeds; on failure keep it
    // open and surface a toast so the unsaved edits stay visible.
    patchRuleStrict(ruleID, {config: cfg}).then(function() {
      if (panel) panel.classList.add('hidden');
    }).catch(function() {
      if (typeof showToast === 'function') {
        showToast('Failed to update rule.');
      }
    });
  }

  function applyResPreset(select) {
    var val = select.value;
    if (!val || val === 'custom') return;
    var parts = val.split(',');
    var form = select.closest('form');
    var w = form.elements['min_width'];
    var h = form.elements['min_height'];
    if (w && parts[0]) w.value = parts[0];
    if (h && parts.length > 1 && parts[1]) h.value = parts[1];
  }

  function applyAspectPreset(select) {
    var val = select.value;
    if (!val || val === 'custom') return;
    var form = select.closest('form');
    var ar = form.elements['aspect_ratio'];
    if (ar) ar.value = val;
  }

  // Inline-handler globals: rule-card markup calls these by bare name.
  window.toggleRuleEnabled = toggleRuleEnabled;
  window.runRule = runRule;
  window.handleRuleConfigSubmit = handleRuleConfigSubmit;
  window.applyResPreset = applyResPreset;
  window.applyAspectPreset = applyAspectPreset;

  // The automation-mode <select> calls patchRule(id, {automation_mode}) inline
  // by bare name with no .catch. Expose a window.patchRule that swallows the
  // rejection with a toast (the pre-extraction behavior) so it neither throws a
  // ReferenceError nor leaks an unhandled promise rejection. Internal callers
  // use patchRuleStrict directly so their optimistic-UI rollback still works.
  window.patchRule = function(ruleID, changes) {
    return patchRuleStrict(ruleID, changes).catch(function() {
      if (typeof showToast === 'function') {
        showToast('Failed to update rule.');
      }
    });
  };

  window.swRuleToggle = {
    toggleEnabled: toggleRuleEnabled,
    run: runRule,
    handleConfigSubmit: handleRuleConfigSubmit
  };
})();
