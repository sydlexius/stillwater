// Settings: extracted from the inline profileNamingEditorScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): naming-save-status
// Network: /api/v1/platforms/
//
// Export surface: window.swProfileNaming doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: addNamingChip, saveProfileNaming.
(function () {
  'use strict';

  if (window.swProfileNaming) return;

      function addNamingChip(btn, imageType) {
        var name = prompt('Enter filename (e.g. folder.jpg):');
        if (!name) return;
        name = name.trim();
        if (!name) return;

        // Client-side validation
        if (name.indexOf('/') !== -1 || name.indexOf('\\') !== -1) {
          alert('Filename must not contain path separators.');
          return;
        }
        var dotIdx = name.lastIndexOf('.');
        if (dotIdx === -1) {
          alert('Filename must have an extension (.jpg, .jpeg or .png).');
          return;
        }
        var ext = name.substring(dotIdx).toLowerCase();
        if (ext !== '.jpg' && ext !== '.jpeg' && ext !== '.png') {
          alert('Extension must be .jpg, .jpeg or .png.');
          return;
        }
        if (imageType === 'logo' && ext !== '.png') {
          alert('Logo filenames must use .png extension.');
          return;
        }

        // Check for duplicates
        var container = btn.closest('[data-naming-type]');
        var chips = container.querySelectorAll('[data-naming-chip]');
        for (var i = 0; i < chips.length; i++) {
          if (chips[i].dataset.namingChip.toLowerCase() === name.toLowerCase()) {
            alert('Duplicate filename.');
            return;
          }
        }

        var chip = document.createElement('span');
        chip.className = 'inline-flex items-center gap-1 rounded-full bg-gray-100 dark:bg-gray-700 px-2.5 py-0.5 text-xs font-medium text-gray-700 dark:text-gray-300';
        chip.dataset.namingChip = name;
        chip.textContent = name;
        var removeBtn = document.createElement('button');
        removeBtn.type = 'button';
        removeBtn.className = 'ml-0.5 text-gray-400 hover:text-red-500 focus:outline-none';
        removeBtn.setAttribute('aria-label', 'Remove ' + name);
        removeBtn.innerHTML = '&times;';
        removeBtn.onclick = function() { chip.remove(); };
        chip.appendChild(removeBtn);
        container.insertBefore(chip, btn);
      }

      function saveProfileNaming(btn) {
        var profileId = btn.dataset.profileId;
        var payload = {};
        var types = ['thumb', 'fanart', 'logo', 'banner'];
        for (var i = 0; i < types.length; i++) {
          var container = document.querySelector('[data-naming-type="' + types[i] + '"]');
          var names = [];
          if (container) {
            var chips = container.querySelectorAll('[data-naming-chip]');
            for (var j = 0; j < chips.length; j++) {
              names.push(chips[j].dataset.namingChip);
            }
          }
          payload[types[i]] = names;
        }

        var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
        var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
        var status = document.getElementById('naming-save-status');
        status.textContent = 'Saving...';

        fetch(bp + '/api/v1/platforms/' + profileId, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': csrfToken
          },
          body: JSON.stringify({ image_naming: payload }),
          credentials: 'same-origin'
        }).then(function(r) {
          if (r.ok) {
            status.textContent = 'Saved.';
            setTimeout(function() { status.textContent = ''; }, 2000);
          } else {
            r.json().then(function(data) {
              var msg = data.error || 'Save failed.';
              if (data.details) msg += ' ' + data.details.join('; ');
              status.textContent = msg;
            }).catch(function() {
              status.textContent = 'Save failed.';
            });
          }
        }).catch(function() {
          status.textContent = 'Network error.';
        });
      }

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.addNamingChip = addNamingChip;
  window.saveProfileNaming = saveProfileNaming;

  window.swProfileNaming = { addNamingChip: addNamingChip, saveProfileNaming: saveProfileNaming };
})();
