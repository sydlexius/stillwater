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
//   onclick="togglePrunePlatformCopies(this)" -- role=switch with
//     data-prune-platform-copies (image_duplicate form). Local state only;
//     handleRuleConfigSubmit reads it on Save. data-initial = stored value,
//     data-prune-blocked = refused tolerance, data-prune-confirm-* = dialog copy.
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

  // applyPruneSwitch paints the "also delete on media servers" switch on or
  // off, using the class strings the template supplies (data-sw-btn-*,
  // data-sw-knob-*). A switch whose tolerance the server cleanup refuses
  // (data-prune-blocked) is locked while off and stored off: dimmed and
  // aria-disabled, because turning it on would be a consent that does nothing.
  function applyPruneSwitch(sw, on) {
    sw.setAttribute('aria-checked', String(on));
    sw.setAttribute('class', on ? sw.dataset.swBtnOn : sw.dataset.swBtnOff);
    var knob = sw.querySelector('span');
    if (knob) knob.setAttribute('class', on ? sw.dataset.swKnobOn : sw.dataset.swKnobOff);
    var locked = sw.hasAttribute('data-prune-blocked') && !on && sw.dataset.initial !== 'true';
    sw.classList.toggle('opacity-50', locked);
    sw.classList.toggle('cursor-not-allowed', locked);
    if (locked) sw.classList.remove('cursor-pointer');
    if (locked) {
      sw.setAttribute('aria-disabled', 'true');
    } else {
      sw.removeAttribute('aria-disabled');
    }
  }

  // togglePrunePlatformCopies flips the switch on screen only. Nothing is sent
  // until Save, like every other field in the config form.
  function togglePrunePlatformCopies(sw) {
    var isOn = sw.getAttribute('aria-checked') === 'true';
    // Refused tolerance: turning off always works. Turning on is allowed only
    // to undo an unsaved turn-off of an option that is stored on.
    if (!isOn && sw.hasAttribute('data-prune-blocked') && sw.dataset.initial !== 'true') return;
    applyPruneSwitch(sw, !isOn);
  }

  function handleRuleConfigSubmit(event) {
    event.preventDefault();
    var form = event.target;
    // One save per form at a time, so a late response from an older save
    // cannot repaint the switch from state captured before the wait.
    if (form.dataset.inflight === '1') {
      console.error('rule-toggle: a save is already in progress; this Save was dropped');
      if (typeof showToast === 'function') showToast('A save is already in progress. Try again in a moment.');
      return;
    }
    var ruleID = form.dataset.ruleId;
    var cfg = {};
    var intFields = {'min_width':1, 'min_height':1, 'min_length':1, 'trim_margin':1};
    ['min_width', 'min_height', 'aspect_ratio', 'tolerance', 'min_length', 'threshold_percent', 'trim_margin', 'coverage_threshold'].forEach(function(f) {
      var el = form.elements[f];
      if (el && el.value !== '') cfg[f] = intFields[f] ? parseInt(el.value, 10) : parseFloat(el.value);
    });
    // Text-valued config fields (e.g. the discography release-type filter) are
    // forwarded as-is rather than parsed as numbers.
    ['release_types'].forEach(function(f) {
      var el = form.elements[f];
      if (!el) return;
      var value = el.value.trim();
      if (value !== '') cfg[f] = value;
    });
    // The provider_id_missing rule renders its required-provider set as one
    // checkbox per available in-scope provider (name="required_provider_ids").
    // Storage mirrors the checker's dynamic-default semantics (config replaces
    // the whole RuleConfig; required_provider_ids is omitempty, so an omitted
    // value persists as empty):
    //   - all available checked, no unrendered override -> omit, storing empty
    //     so the rule keeps requiring every available in-scope provider as
    //     they are added/removed.
    //   - a strict subset checked, or an unrendered override exists -> store
    //     the union of checked-visible + unrendered-stored, comma-separated.
    //   - none of the rendered boxes checked -> reject the save. An empty
    //     string means "all", so silently storing it would invert the
    //     operator's intent; require at least one provider (or disable the
    //     rule to require none).
    // The fieldset's data-stored-provider-ids (settings.templ) carries the
    // raw persisted override so a provider that is no longer rendered (its
    // key/availability was removed) is not silently dropped from the stored
    // requirement on the next save -- it is merged back in as "unrendered".
    var providerBoxes = form.querySelectorAll('input[type="checkbox"][name="required_provider_ids"]');
    var providerFieldset = form.querySelector('fieldset[data-stored-provider-ids]');
    var storedProviderIDs = providerFieldset ? providerFieldset.dataset.storedProviderIds : '';
    if (providerBoxes.length > 0 || storedProviderIDs) {
      var visibleProviders = {};
      var checkedProviders = [];
      providerBoxes.forEach(function(box) {
        visibleProviders[box.value] = true;
        if (box.checked) checkedProviders.push(box.value);
      });
      if (providerBoxes.length > 0 && checkedProviders.length === 0) {
        if (typeof showToast === 'function') {
          showToast('Select at least one provider, or disable the rule.');
        }
        return;
      }
      var unrenderedStored = [];
      if (storedProviderIDs) {
        storedProviderIDs.split(',').forEach(function(tok) {
          var name = tok.trim().toLowerCase();
          if (name && !visibleProviders[name] && unrenderedStored.indexOf(name) === -1) {
            unrenderedStored.push(name);
          }
        });
      }
      var allVisibleCheckedNoOverride = providerBoxes.length > 0 &&
        checkedProviders.length === providerBoxes.length &&
        unrenderedStored.length === 0;
      if (!allVisibleCheckedNoOverride) {
        var finalProviders = checkedProviders.concat(unrenderedStored);
        if (finalProviders.length > 0) {
          cfg['required_provider_ids'] = finalProviders.join(',');
        }
      }
    }
    var sev = form.elements['severity'];
    if (sev) cfg['severity'] = sev.value;
    // image_duplicate only: the "also delete on media servers" switch. The key
    // is sent only when on; the API replaces the whole config and the field is
    // omitempty, so leaving it out stores the option as off.
    var pruneSw = form.querySelector('[data-prune-platform-copies]');
    var pruneOn = !!pruneSw && pruneSw.getAttribute('aria-checked') === 'true';
    if (pruneOn) cfg['prune_platform_copies'] = true;
    var panel = document.getElementById('rule-cfg-' + ruleID);
    function failToast() {
      if (typeof showToast === 'function') {
        showToast('Failed to update rule.');
      } else {
        console.error('rule-toggle: showToast unavailable; the rule config was not saved');
      }
    }
    // Only collapse the config panel once the save succeeds; on failure keep it
    // open and surface a toast so the unsaved edits stay visible.
    function save() {
      form.dataset.inflight = '1';
      patchRuleStrict(ruleID, {config: cfg}).then(function() {
        if (pruneSw) {
          // The stored value is now what the switch shows; keep data-initial
          // (which drives the refused-threshold lock) in step with it.
          pruneSw.dataset.initial = String(pruneOn);
          applyPruneSwitch(pruneSw, pruneOn);
        }
        if (panel) panel.classList.add('hidden');
      }).catch(function(err) {
        console.error('rule-toggle: saving the rule config failed:', err);
        failToast();
      }).then(function() {
        delete form.dataset.inflight;
      });
    }
    // Every save with the switch on needs consent, whatever the stored value
    // was at page load (it may have changed since). Each refusal below sends
    // NOTHING and leaves the panel open. A refused tolerance cannot be saved
    // on; turning the switch OFF never reaches this code and always saves.
    if (!pruneOn) {
      save();
      return;
    }
    if (pruneSw.hasAttribute('data-prune-blocked')) {
      // The server cleanup refuses this tolerance (the note beside the switch
      // says so). Do not store a consent that does nothing.
      var refused = 'Not saved. The server cleanup cannot run at this similarity threshold. Turn the switch off to save.';
      console.error('rule-toggle: ' + refused);
      if (typeof showToast === 'function') showToast(refused);
      return;
    }
    if (typeof window.showConfirmDialog !== 'function') {
      // Fail closed: without the dialog there is no consent, so do not save.
      console.error('rule-toggle: showConfirmDialog unavailable; refusing to turn on media-server deletion without confirmation');
      failToast();
      return;
    }
    // A null key means no "Don't ask again": consent to deleting on a remote
    // server is asked for every time.
    window.showConfirmDialog(pruneSw.dataset.pruneConfirmBody, null, save, {
      title: pruneSw.dataset.pruneConfirmTitle,
      acceptText: pruneSw.dataset.pruneConfirmAccept
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
  window.togglePrunePlatformCopies = togglePrunePlatformCopies;
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
