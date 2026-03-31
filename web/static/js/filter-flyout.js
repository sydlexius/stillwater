// Filter flyout panel controller.
// Manages open/close state, three-state item cycling, URL query-param sync,
// and HTMX content refresh on apply.
//
// Public API (exposed as window.swFilterFlyout):
//   open(id)       -- open the flyout with the given panel id
//   close(id)      -- close the flyout and return focus to the trigger element
//   cycleItem(el)  -- cycle a FilterItem through neutral -> include -> exclude -> neutral
//   apply(id)      -- write active filter state to URL and trigger HTMX reload
//   clearAll(id)   -- reset all items to neutral and clear URL params for this flyout
(function () {
  'use strict';

  // getPanel returns the panel element by id.
  function getPanel(id) {
    return document.getElementById(id);
  }

  // getScrim returns the scrim element associated with a panel.
  function getScrim(id) {
    return document.getElementById(id + '-scrim');
  }

  // open shows the flyout panel and its scrim, moves focus to the first
  // interactive control inside the body, and marks the panel accessible.
  function open(id) {
    var panel = getPanel(id);
    var scrim = getScrim(id);
    if (!panel) return;

    panel.classList.add('sw-filter-flyout--open');
    panel.setAttribute('aria-hidden', 'false');
    panel.removeAttribute('inert');
    if (scrim) {
      scrim.classList.add('sw-filter-scrim--visible');
    }

    var triggerID = panel.getAttribute('data-trigger-id');
    if (triggerID) {
      var trigger = document.getElementById(triggerID);
      if (trigger) trigger.setAttribute('aria-expanded', 'true');
    }

    // Move focus to the first focusable element inside the body.
    var body = panel.querySelector('.sw-filter-flyout-body');
    if (body) {
      var first = body.querySelector(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
      );
      if (first) {
        first.focus();
      } else {
        // Fall back to close button so focus always lands inside the panel.
        var closeBtn = panel.querySelector('.sw-filter-flyout-close');
        if (closeBtn) closeBtn.focus();
      }
    }
  }

  // close hides the flyout panel and scrim and returns focus to the element
  // that triggered the flyout (stored in data-trigger-id).
  function close(id) {
    var panel = getPanel(id);
    var scrim = getScrim(id);
    if (!panel) return;

    panel.classList.remove('sw-filter-flyout--open');
    panel.setAttribute('aria-hidden', 'true');
    panel.setAttribute('inert', '');
    if (scrim) {
      scrim.classList.remove('sw-filter-scrim--visible');
    }

    // Return focus to the trigger element and collapse its aria-expanded state.
    var triggerID = panel.getAttribute('data-trigger-id');
    if (triggerID) {
      var trigger = document.getElementById(triggerID);
      if (trigger) {
        trigger.setAttribute('aria-expanded', 'false');
        trigger.focus();
      }
    }
  }

  // cycleItem advances a FilterItem through neutral -> include -> exclude -> neutral.
  // Updates aria-label, icon text, and data-filter-state on the element.
  function cycleItem(el) {
    var state = el.getAttribute('data-filter-state') || 'neutral';
    var label = el.querySelector('.sw-filter-item-label');
    var labelText = label ? label.textContent.trim() : '';
    var icon = el.querySelector('.sw-filter-item-icon');

    var next;
    switch (state) {
      case 'neutral':  next = 'include'; break;
      case 'include':  next = 'exclude'; break;
      default:         next = 'neutral'; break;
    }

    el.setAttribute('data-filter-state', next);
    el.setAttribute('aria-label', ariaLabel(next, labelText));
    if (icon) {
      icon.textContent = iconChar(next);
    }

    // Update the active count badge in the footer.
    var flyoutID = el.getAttribute('data-filter-flyout');
    if (flyoutID) {
      refreshActiveCount(flyoutID);
    }
  }

  // ariaLabel returns the accessible label string for a given state + label.
  function ariaLabel(state, label) {
    switch (state) {
      case 'include': return 'Include ' + label + ' (click to exclude)';
      case 'exclude': return 'Exclude ' + label + ' (click to clear)';
      default:        return 'Any ' + label + ' (click to include)';
    }
  }

  // iconChar returns the icon character for a given state.
  function iconChar(state) {
    switch (state) {
      case 'include': return '+';
      case 'exclude': return '-';
      default:        return '';
    }
  }

  // refreshActiveCount counts non-neutral items in the flyout and updates the
  // active-count badge in the footer.
  function refreshActiveCount(id) {
    var panel = getPanel(id);
    if (!panel) return;

    var items = panel.querySelectorAll('[data-filter-state]');
    var count = 0;
    Array.prototype.forEach.call(items, function (item) {
      var s = item.getAttribute('data-filter-state');
      if (s === 'include' || s === 'exclude') count++;
    });

    var badge = panel.querySelector('.sw-filter-active-badge');
    if (badge) {
      badge.textContent = count > 0 ? count + ' active' : '';
    }
  }

  // buildParams reads all FilterItem states in the panel and returns an object
  // mapping param key -> array of values with a state prefix ("+" or "-").
  // Example: { severity: ["+error", "-warning"] }
  function buildParams(id) {
    var panel = getPanel(id);
    if (!panel) return {};

    var params = {};
    var items = panel.querySelectorAll('[data-filter-key][data-filter-value][data-filter-state]');
    Array.prototype.forEach.call(items, function (item) {
      var state = item.getAttribute('data-filter-state');
      if (state === 'neutral') return;
      var key = item.getAttribute('data-filter-key');
      var value = item.getAttribute('data-filter-value');
      var prefixed = (state === 'include' ? '+' : '-') + value;
      if (!params[key]) params[key] = [];
      params[key].push(prefixed);
    });
    return params;
  }

  // apply writes the current filter state to the URL query string and triggers
  // an HTMX reload of the target element identified by data-target-sel.
  function apply(id) {
    var panel = getPanel(id);
    if (!panel) return;

    var params = buildParams(id);
    var url = new URL(window.location.href);

    // Remove all existing filter params managed by this flyout (they carry
    // the data-filter-key values). Use a plain object instead of Set for ES5
    // compatibility.
    var allKeys = {};
    Array.prototype.forEach.call(
      panel.querySelectorAll('[data-filter-key]'),
      function (el) {
        allKeys[el.getAttribute('data-filter-key')] = true;
      }
    );
    Object.keys(allKeys).forEach(function (k) { url.searchParams.delete(k); });

    // Write new params.
    Object.keys(params).forEach(function (key) {
      params[key].forEach(function (val) {
        url.searchParams.append(key, val);
      });
    });

    history.pushState(null, '', url.toString());

    // Trigger HTMX reload of the target region.
    var targetSel = panel.getAttribute('data-target-sel');
    if (targetSel && window.htmx) {
      var target = document.querySelector(targetSel);
      if (target) htmx.trigger(target, 'sw:filter-applied');
    }

    close(id);
  }

  // clearAll resets all FilterItems in the panel to neutral, clears their URL
  // params, and closes the flyout.
  function clearAll(id) {
    var panel = getPanel(id);
    if (!panel) return;

    Array.prototype.forEach.call(
      panel.querySelectorAll('[data-filter-state]'),
      function (item) {
        var label = item.querySelector('.sw-filter-item-label');
        var labelText = label ? label.textContent.trim() : '';
        var icon = item.querySelector('.sw-filter-item-icon');
        item.setAttribute('data-filter-state', 'neutral');
        item.setAttribute('aria-label', ariaLabel('neutral', labelText));
        if (icon) icon.textContent = '';
      }
    );

    refreshActiveCount(id);

    // Remove filter params from the URL.
    var url = new URL(window.location.href);
    Array.prototype.forEach.call(
      panel.querySelectorAll('[data-filter-key]'),
      function (el) {
        url.searchParams.delete(el.getAttribute('data-filter-key'));
      }
    );
    history.pushState(null, '', url.toString());

    // Trigger HTMX reload so the cleared state is reflected.
    var targetSel = panel.getAttribute('data-target-sel');
    if (targetSel && window.htmx) {
      var target = document.querySelector(targetSel);
      if (target) htmx.trigger(target, 'sw:filter-applied');
    }

    close(id);
  }

  // initFromURL reads URL query params on page load and sets each FilterItem's
  // initial state from any pre-existing filter params.  Called once the DOM is
  // ready.  This function is idempotent; calling it again after navigation is
  // safe.
  function initFromURL(id) {
    var panel = getPanel(id);
    if (!panel) return;

    var url = new URL(window.location.href);
    Array.prototype.forEach.call(
      panel.querySelectorAll('[data-filter-key][data-filter-value]'),
      function (item) {
        var key = item.getAttribute('data-filter-key');
        var value = item.getAttribute('data-filter-value');
        var label = item.querySelector('.sw-filter-item-label');
        var labelText = label ? label.textContent.trim() : '';
        var icon = item.querySelector('.sw-filter-item-icon');

        var state = 'neutral';
        var vals = url.searchParams.getAll(key);
        if (vals.indexOf('+' + value) !== -1) {
          state = 'include';
        } else if (vals.indexOf('-' + value) !== -1) {
          state = 'exclude';
        }

        item.setAttribute('data-filter-state', state);
        item.setAttribute('aria-label', ariaLabel(state, labelText));
        if (icon) icon.textContent = iconChar(state);
      }
    );

    refreshActiveCount(id);
  }

  // Keyboard handling: Escape closes the open flyout; Tab/Shift+Tab traps
  // focus inside the open flyout panel.
  document.addEventListener('keydown', function (e) {
    var openPanel = document.querySelector('.sw-filter-flyout.sw-filter-flyout--open');

    if (e.key === 'Escape') {
      if (openPanel) close(openPanel.id);
      return;
    }

    // Focus trap: when the flyout is open, Tab/Shift+Tab cycle only within
    // the panel's focusable elements.
    if (openPanel && e.key === 'Tab') {
      var focusableNodes = openPanel.querySelectorAll(
        'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), ' +
        'textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
      );
      var focusable = Array.prototype.slice.call(focusableNodes).filter(function (el) {
        return el.offsetParent !== null;
      });
      if (focusable.length === 0) return;
      var first = focusable[0];
      var last = focusable[focusable.length - 1];
      if (e.shiftKey) {
        if (document.activeElement === first) {
          e.preventDefault();
          last.focus();
        }
      } else {
        if (document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    }
  });

  // Expose public API.
  window.swFilterFlyout = {
    open: open,
    close: close,
    cycleItem: cycleItem,
    apply: apply,
    clearAll: clearAll,
    initFromURL: initFromURL,
    refreshActiveCount: refreshActiveCount
  };
}());
