// Regression test for the localStorage.getItem guard in the column-toggle
// component (web/components/column_toggle.templ, issue #1964).
//
// The inline IIFE script runs on every page load via initAllColumnToggles().
// In storage-blocked / private-mode contexts (Safari "block all cookies",
// sandboxed iframes), localStorage.getItem throws a SecurityError. This test
// confirms applyColumnVisibility / initAllColumnToggles do NOT throw and that
// init completes normally when getItem throws.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { JSDOM } from 'jsdom';

// The column-toggle IIFE is embedded in column_toggle.templ (not a standalone
// static JS file). We inline it here as a literal string so the test does not
// depend on parsing the templ/Go generated output. It must be kept in sync
// with the production script in web/components/column_toggle.templ.
const COLUMN_TOGGLE_SCRIPT = `
(function() {
	if (window.__colToggleInit) return;
	window.__colToggleInit = true;

	document.addEventListener('click', function(e) {
		document.querySelectorAll('[data-col-toggle]').forEach(function(wrapper) {
			if (!wrapper.contains(e.target)) {
				var menu = wrapper.querySelector('[data-col-menu]');
				if (menu) menu.classList.add('hidden');
			}
		});
	});

	window.handleColumnToggle = function(checkbox) {
		var wrapper = checkbox.closest('[data-col-toggle]');
		if (!wrapper) return;
		var storageKey = wrapper.getAttribute('data-col-toggle');
		var tableID = wrapper.getAttribute('data-col-table');
		var key = checkbox.getAttribute('data-col-key');
		var table = document.getElementById(tableID);
		if (!table) return;

		try {
			var cells = table.querySelectorAll('[data-col="' + key + '"]');
			cells.forEach(function(cell) {
				cell.style.display = checkbox.checked ? '' : 'none';
			});
		} catch(e) { /* skip invalid selector from corrupted key */ }

		saveColumnState(storageKey, wrapper);
	};

	function saveColumnState(storageKey, wrapper) {
		var hidden = [];
		wrapper.querySelectorAll('input[data-col-key]').forEach(function(cb) {
			if (!cb.checked && !cb.disabled) {
				hidden.push(cb.getAttribute('data-col-key'));
			}
		});
		try {
			localStorage.setItem('columns.' + storageKey, JSON.stringify(hidden));
		} catch (e) {
			console.warn('[column_toggle] column state not persisted (private mode/quota):', e);
		}
	}

	function applyColumnVisibility(storageKey, tableID, wrapper) {
		var raw;
		try {
			raw = localStorage.getItem('columns.' + storageKey);
		} catch (e) {
			console.warn('[column_toggle] column state not accessible (private mode/quota):', e);
			return;
		}
		if (!raw) return;

		var hidden;
		try { hidden = JSON.parse(raw); } catch(e) { return; }
		if (!Array.isArray(hidden)) return;

		var table = document.getElementById(tableID);
		if (!table) return;

		hidden.forEach(function(key) {
			try {
				table.querySelectorAll('[data-col="' + key + '"]').forEach(function(cell) {
					cell.style.display = 'none';
				});
			} catch(e) { /* skip invalid selector from corrupted key */ }
		});

		if (wrapper) {
			wrapper.querySelectorAll('input[data-col-key]').forEach(function(cb) {
				var k = cb.getAttribute('data-col-key');
				cb.checked = hidden.indexOf(k) === -1;
			});
		}
	}

	window.initAllColumnToggles = function() {
		document.querySelectorAll('[data-col-toggle]').forEach(function(wrapper) {
			var sk = wrapper.getAttribute('data-col-toggle');
			var tid = wrapper.getAttribute('data-col-table');
			applyColumnVisibility(sk, tid, wrapper);
		});
	};

	initAllColumnToggles();

	document.body.addEventListener('htmx:afterSettle', function() {
		initAllColumnToggles();
	});
})();
`;

// Minimal page with a column-toggle wrapper + table, matching the structure
// the production component renders.
const PAGE_HTML = `<!doctype html><html><body>
<div data-col-toggle="artists" data-col-table="artists-table">
  <button type="button">Columns</button>
  <div data-col-menu class="hidden">
    <label>
      <input type="checkbox" checked data-col-key="name">Name
    </label>
    <label>
      <input type="checkbox" checked data-col-key="genre">Genre
    </label>
  </div>
</div>
<table id="artists-table">
  <tr>
    <th data-col="name">Name</th>
    <th data-col="genre">Genre</th>
  </tr>
</table>
</body></html>`;

// Create a jsdom window with localStorage replaced by a throwing stub that
// simulates the SecurityError raised in storage-blocked / private-mode
// contexts (Safari "block all cookies", sandboxed iframes).
function setupWithThrowingStorage() {
  const dom = new JSDOM(PAGE_HTML, {
    runScripts: 'dangerously',
    url: 'http://localhost:1973/',
  });
  const win = dom.window;

  // Replace localStorage with a stub that throws on every access, matching
  // the SecurityError a storage-blocked context raises.
  Object.defineProperty(win, 'localStorage', {
    get() {
      throw new win.DOMException('Storage access blocked', 'SecurityError');
    },
    configurable: true,
  });

  return dom;
}

describe('column-toggle: localStorage.getItem guard (private mode / storage blocked)', () => {
  it('initAllColumnToggles does not throw when localStorage.getItem raises SecurityError', () => {
    const dom = setupWithThrowingStorage();
    const win = dom.window;

    // Eval the IIFE. The IIFE itself calls initAllColumnToggles() at the end,
    // so a throw during getItem would surface here if unguarded.
    assert.doesNotThrow(() => {
      win.eval(COLUMN_TOGGLE_SCRIPT);
    }, 'IIFE init (including initAllColumnToggles call) must not throw');

    // initAllColumnToggles must also be safe to call again (e.g. after HTMX
    // afterSettle), which re-enters applyColumnVisibility.
    assert.doesNotThrow(() => {
      win.initAllColumnToggles();
    }, 'subsequent initAllColumnToggles call must not throw');
  });

  it('columns remain visible (default state) when getItem throws', () => {
    const dom = setupWithThrowingStorage();
    const win = dom.window;

    win.eval(COLUMN_TOGGLE_SCRIPT);

    // No column should be hidden: the guard returns early before applying any
    // stored hidden list, so all columns stay at their default display.
    const table = win.document.getElementById('artists-table');
    const cells = table.querySelectorAll('[data-col]');
    cells.forEach(cell => {
      assert.notEqual(
        cell.style.display,
        'none',
        `column "${cell.getAttribute('data-col')}" must not be hidden when storage is blocked`,
      );
    });
  });
});
