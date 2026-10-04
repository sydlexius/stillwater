// Settings: Provider-priority drag reorder (M55 #1808).
// Extracted verbatim from the inline sortableInitScript() that used to live in
// web/templates/settings.templ. Attaches Sortable.js to every provider-priority
// container and persists the new order on drop. Re-runs after each HTMX swap so
// freshly rendered priority rows become draggable immediately.
//
// DOM contract (provider-priority cards in settings.templ):
//   [data-sortable-field]   -- a draggable container; dataset.sortableField is
//                              the priority field name sent to the API.
//   .drag-handle             -- the drag affordance within each chip.
//   [data-provider]          -- enabled provider chip; dataset.provider is sent.
//   [data-disabled-provider] -- disabled provider chip; preserved in the saved
//                              order so it is not dropped from the list.
//   [data-hidden-provider]   -- hidden marker for a stored provider rendered as no
//                              chip (capability-filtered or no key); preserved in the
//                              saved order so a reorder does not delete it (#3190).
//   [id^="priority-row-"]    -- the row wrapper used to find disabled/hidden entries.
//
// Cross-script contract: depends on the global Sortable (Sortable.min.js) and the
// optional global showToast (probed with typeof). Sortable is NOT guaranteed to
// precede this module in document order: the single app-wide copy is emitted by
// LayoutGlobalChrome AFTER the page content, so the initial init is deferred to
// DOMContentLoaded (#2189).
//
// Network contract (base-path aware via meta[name="htmx-base-path"]):
//   PUT {base}/api/v1/providers/priorities -- {priorities:[{field,providers}]}
// The csrf_token cookie is sent as X-CSRF-Token, read via the canonical
// window.swCsrfToken() helper from preferences.js (loaded in layout.templ before
// this module) rather than an inline cookie-parse regex.
//
// Export surface: window.swSortableInit doubles as the load-once guard. No
// inline-handler globals are leaked; initialization is self-contained.
(function () {
  'use strict';

  // Re-init guard: the single window.swSortableInit export (assigned at the
  // bottom) doubles as the "already loaded" flag.
  if (window.swSortableInit) return;

  var sortableBp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;

  // initSortableContainers attaches Sortable.js to all [data-sortable-field] containers
  // that do not already have a Sortable instance. Called on initial page load and after
  // every HTMX swap so that newly rendered priority rows are draggable immediately.
  function initSortableContainers() {
    if (typeof Sortable === 'undefined') {
      console.error('sortable-init: SortableJS not loaded; provider priority drag-reorder disabled');
      return;
    }
    document.querySelectorAll('[data-sortable-field]').forEach(function(container) {
      if (container._sortable) return; // already initialised
      var field = container.dataset.sortableField;
      container._sortable = Sortable.create(container, {
        handle: '.drag-handle',
        animation: 150,
        ghostClass: 'opacity-30',
        dragClass: 'shadow-lg',
        onChoose: function(evt) {
          evt.item.classList.add('ring-2', 'ring-blue-400', 'rounded-full');
        },
        onUnchoose: function(evt) {
          evt.item.classList.remove('ring-2', 'ring-blue-400', 'rounded-full');
        },
        onEnd: function() {
          var providers = [];
          container.querySelectorAll('[data-provider]').forEach(function(chip) {
            providers.push(chip.dataset.provider);
          });
          // Include disabled providers so they are not dropped from the persisted list.
          var row = container.closest('[id^="priority-row-"]');
          if (row) {
            row.querySelectorAll('[data-disabled-provider]').forEach(function(chip) {
              providers.push(chip.dataset.disabledProvider);
            });
            // Hidden stored providers go last, in stored order, so they survive the save.
            row.querySelectorAll('[data-hidden-provider]').forEach(function(marker) {
              providers.push(marker.dataset.hiddenProvider);
            });
          }
          var csrfToken;
          if (typeof window.swCsrfToken === 'function') {
            csrfToken = window.swCsrfToken();
          } else {
            console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
            csrfToken = '';
          }
          fetch(sortableBp + '/api/v1/providers/priorities', {
            method: 'PUT',
            headers: {
              'Content-Type': 'application/json',
              'X-CSRF-Token': csrfToken
            },
            body: JSON.stringify({
              priorities: [{field: field, providers: providers}]
            }),
            credentials: 'same-origin'
          }).then(function(r) {
            if (!r.ok) {
              if (typeof showToast === 'function') {
                showToast('Failed to save provider order');
              }
              window.location.reload();
            }
          }).catch(function() {
            if (typeof showToast === 'function') {
              showToast('Failed to save provider order');
            }
            // Restore the server's canonical order after a failed reorder by
            // refreshing just the priorities section, not the whole page (M55
            // #1339) -- preserves next/ scroll position + ambient backdrop.
            if (typeof window.swRefreshSettingsSection === 'function') {
              window.swRefreshSettingsSection('priorities').then(function(ok){ if (!ok) window.location.reload(); });
            } else {
              window.location.reload();
            }
          });
        }
      });
    });
  }
  // Initial setup on page load. Deferred past parse so the layout's Sortable
  // tag (which follows this script) has executed; a late-mounted script (readyState
  // already past loading) runs immediately.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initSortableContainers);
  } else {
    initSortableContainers();
  }
  // Re-attach after any HTMX swap so newly rendered priority rows are draggable.
  document.addEventListener('htmx:afterSwap', function() {
    initSortableContainers();
  });

  window.swSortableInit = {
    init: initSortableContainers
  };
})();
