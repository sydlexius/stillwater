// Tests for the g-leader navigation, '?' cheat-sheet dispatch, and Esc
// cheat-sheet close added to keyboard.js (#1775 PR1). Follows the
// keyboard-register.test.js pattern: node:test + dom-harness createDom.
import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createDom, flush } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const KEYBOARD_PATH = resolve(__dirname, '../../web/static/js/keyboard.js');

// setup: create a fresh jsdom with keyboard.js loaded.
// opts.leaderTimeout overrides LEADER_TIMEOUT_MS via window.SW_LEADER_TIMEOUT_MS.
// opts.isNextPage controls the SW_IS_NEXT_PAGE seam (defaults true so
// g-leader/? handlers are active; pass false to simulate stable channel).
function setup(extraHtml = '', opts = {}) {
  const dom = createDom({
    html: `<!doctype html><html><body>
      <input id="search" type="text" data-sw-shortcut="/" data-sw-shortcut-label="Search">
      ${extraHtml}
    </body></html>`,
    modules: [],
  });
  const win = dom.window;

  // Navigation seam: tests set win.swNavigate to capture calls; production
  // path falls back to window.location.href assignment.
  const navigated = [];
  win.swNavigate = (url) => navigated.push(url);

  // Leader-timeout override (fast tests).
  if (opts.leaderTimeout != null) {
    win.SW_LEADER_TIMEOUT_MS = opts.leaderTimeout;
  }

  // Channel-gate seam: jsdom runs at http://localhost:1973/ (not /next/) so
  // isNextPage() would return false without this override. Default to true so
  // g-leader/? tests exercise the next/ active path; pass false for stable tests.
  win.SW_IS_NEXT_PAGE = opts.isNextPage !== false;

  // Load keyboard.js after setting any seam flags.
  win.eval(readFileSync(KEYBOARD_PATH, 'utf-8'));
  win.swKeyboardShortcuts.rebuild();

  return { win, navigated };
}

// fire: dispatch a KeyboardEvent on document and return the event.
function fire(win, key, extra = {}) {
  const evt = new win.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...extra });
  win.document.dispatchEvent(evt);
  return evt;
}

describe('keyboard.js: g-leader navigation (#1775)', () => {
  it('pressing g arms the leader (no navigation yet)', () => {
    const { win, navigated } = setup('', { leaderTimeout: 1 });
    fire(win, 'g');
    assert.equal(navigated.length, 0, 'g alone must not navigate');
  });

  it('g then d navigates to /next/', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'd');
    assert.equal(navigated.length, 1, 'g+d must trigger one navigation');
    assert.ok(navigated[0].endsWith('/next/'), `expected /next/, got ${navigated[0]}`);
  });

  it('g then a navigates to /next/artists', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'a');
    assert.ok(navigated[0].endsWith('/next/artists'), `expected /next/artists, got ${navigated[0]}`);
  });

  it('g then r navigates to the canonical /reports (#1757 PR-4)', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'r');
    assert.equal(navigated[0], '/reports', `expected /reports, got ${navigated[0]}`);
  });

  it('g then l navigates to the canonical /logs (#1757 PR-5)', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'l');
    assert.equal(navigated[0], '/logs', `expected /logs, got ${navigated[0]}`);
  });

  it('g then f navigates to the canonical /reports (Findings = Reports)', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'f');
    assert.equal(navigated[0], '/reports', `expected /reports, got ${navigated[0]}`);
  });

  it('g then s navigates to the canonical /settings (#1757 PR-5)', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 's');
    assert.equal(navigated[0], '/settings', `expected /settings, got ${navigated[0]}`);
  });

  it('g in a text input is a no-op (leader not armed)', () => {
    const { win, navigated } = setup();
    const input = win.document.getElementById('search');
    input.focus();
    fire(win, 'g');
    // Blur and immediately press d -- should not navigate because leader was not armed.
    input.blur();
    fire(win, 'd');
    assert.equal(navigated.length, 0, 'g in input must not arm the leader');
  });

  it('g then unknown key cancels leader without navigating', () => {
    const { win, navigated } = setup();
    fire(win, 'g');
    fire(win, 'z'); // not in leader target map
    // A subsequent 'd' should not navigate (leader was consumed by 'z').
    fire(win, 'd');
    assert.equal(navigated.length, 0, 'unknown second key must not navigate');
  });

  it('leader clears after timeout, no navigation fires', async () => {
    // Use a 50ms timeout so the test does not wait 1.5s.
    const { win, navigated } = setup('', { leaderTimeout: 50 });
    fire(win, 'g'); // arm leader
    // Wait for timeout to expire.
    await new Promise(r => setTimeout(r, 100));
    fire(win, 'd'); // should be ignored -- leader has expired
    assert.equal(navigated.length, 0, 'navigation must not fire after leader timeout');
  });
});

describe('keyboard.js: ? cheat-sheet dispatch (#1775)', () => {
  it('? (not in input) calls window.showCheatSheet when defined', () => {
    const { win } = setup();
    const calls = [];
    win.showCheatSheet = () => calls.push(1);
    fire(win, '?');
    assert.equal(calls.length, 1, '? must call showCheatSheet when defined');
  });

  it('? in a text input does not call showCheatSheet', () => {
    const { win } = setup();
    const calls = [];
    win.showCheatSheet = () => calls.push(1);
    const input = win.document.getElementById('search');
    input.focus();
    fire(win, '?');
    assert.equal(calls.length, 0, '? in text input must not call showCheatSheet');
  });

  it('? when showCheatSheet is not defined is a no-op and does not throw', () => {
    const { win } = setup();
    // showCheatSheet not set (stable channel simulation)
    delete win.showCheatSheet;
    assert.doesNotThrow(() => fire(win, '?'), '? must not throw when showCheatSheet is undefined');
  });
});

describe('keyboard.js: Esc closes cheat sheet (#1775)', () => {
  // Build DOM with a fake cheat-sheet-modal element to simulate the open state.
  function setupWithModal(open = true) {
    const modalHtml = `<div id="cheat-sheet-modal" class="${open ? '' : 'hidden'}"></div>`;
    const { win, navigated } = setup(modalHtml);
    const calls = [];
    win.hideCheatSheet = () => {
      calls.push(1);
      win.document.getElementById('cheat-sheet-modal').classList.add('hidden');
    };
    return { win, navigated, calls };
  }

  it('Esc calls hideCheatSheet when the modal is visible', () => {
    const { win, calls } = setupWithModal(true);
    fire(win, 'Escape');
    assert.equal(calls.length, 1, 'Esc must call hideCheatSheet when modal is open');
  });

  it('Esc does not call hideCheatSheet when the modal is hidden', () => {
    const { win, calls } = setupWithModal(false);
    fire(win, 'Escape');
    assert.equal(calls.length, 0, 'Esc must not call hideCheatSheet when modal is already hidden');
  });
});

describe('keyboard.js: channel gate - stable page (#1775 B2)', () => {
  it('g does not arm the leader on stable', () => {
    const { win, navigated } = setup('', { isNextPage: false });
    fire(win, 'g');
    fire(win, 'd');
    assert.equal(navigated.length, 0, 'g+d must not navigate on stable channel');
  });

  it('? does not call showCheatSheet on stable', () => {
    const { win } = setup('', { isNextPage: false });
    const calls = [];
    win.showCheatSheet = () => calls.push(1);
    fire(win, '?');
    assert.equal(calls.length, 0, '? must not call showCheatSheet on stable channel');
  });

  it('Esc calls hideCheatSheet on stable now that the modal is unconditionally mounted (#2768)', () => {
    const modalHtml = '<div id="cheat-sheet-modal"></div>'; // no .hidden class = open
    const { win } = setup(modalHtml, { isNextPage: false });
    const calls = [];
    win.hideCheatSheet = () => calls.push(1);
    fire(win, 'Escape');
    assert.equal(calls.length, 1, 'Esc must call hideCheatSheet on stable channel');
  });

  it('next/-only global shortcuts (g-leader, cheat sheet, Esc) not registered on stable, but Cmd-K is (#2768)', () => {
    const { win } = setup('', { isNextPage: false });
    const globals = win.swKeyboardShortcuts.list().filter(e => e.scope === 'global');
    // Cmd-K now works on both channels (#2768), so it is the one global entry
    // still registered on stable; the rest remain next/-only.
    assert.deepEqual([...globals.map(e => e.key)], ['⌘K'],
      'only Cmd-K should be registered as a global shortcut on stable channel');
  });
});

describe('keyboard.js: M1 - g-leader suppressed while cheat sheet is open', () => {
  it('g+d does not navigate while cheat-sheet modal is open', () => {
    const modalHtml = '<div id="cheat-sheet-modal"></div>'; // no .hidden class = open
    const { win, navigated } = setup(modalHtml);
    fire(win, 'g');
    fire(win, 'd');
    assert.equal(navigated.length, 0, 'g+d must not navigate while cheat-sheet modal is open');
  });

  it('g+d navigates after cheat-sheet modal is closed', () => {
    const modalHtml = '<div id="cheat-sheet-modal" class="hidden"></div>';
    const { win, navigated } = setup(modalHtml);
    fire(win, 'g');
    fire(win, 'd');
    assert.equal(navigated.length, 1, 'g+d must navigate when cheat-sheet modal is closed');
    assert.ok(navigated[0].endsWith('/next/'), `expected /next/, got ${navigated[0]}`);
  });
});

describe('keyboard.js: Esc closes cheat sheet on the stable channel (#2768 fold-in)', () => {
  it('Esc closes an open cheat-sheet modal on a stable-channel page (isNextPage: false)', () => {
    const modalHtml = '<div id="cheat-sheet-modal"></div>'; // no .hidden class = open
    const { win } = setup(modalHtml, { isNextPage: false });
    const modal = win.document.getElementById('cheat-sheet-modal');
    // Precondition: the modal must start open, otherwise the assertion below
    // would pass vacuously against an already-closed modal.
    assert.ok(!modal.classList.contains('hidden'), 'precondition: modal must start open');
    win.hideCheatSheet = () => modal.classList.add('hidden');
    fire(win, 'Escape');
    assert.ok(modal.classList.contains('hidden'), 'Esc must close the cheat sheet on the stable channel');
  });
});

describe('keyboard.js: Esc only lets the command palette claim Escape while open (#2768 isOpen fix)', () => {
  // Mirrors the real window.swCommandPalette shape (hide + isOpen), which the
  // jsdom harness does NOT define by default -- a test that omits this setup
  // can never enter the guard under test and passes vacuously.
  function setupWithPaletteAndModal(paletteOpen) {
    const modalHtml = '<div id="cheat-sheet-modal"></div>'; // no .hidden class = open
    const { win } = setup(modalHtml);
    const modal = win.document.getElementById('cheat-sheet-modal');
    const hideCalls = [];
    win.swCommandPalette = {
      hide: () => { hideCalls.push(1); },
      isOpen: () => paletteOpen,
    };
    // Precondition: fail loudly if the harness shape does not match reality,
    // rather than passing without ever entering the isOpen() guard.
    assert.equal(typeof win.swCommandPalette.isOpen, 'function',
      'precondition: window.swCommandPalette.isOpen must be a function');
    win.hideCheatSheet = () => modal.classList.add('hidden');
    return { win, modal, hideCalls };
  }

  it('palette CLOSED (isOpen() false): Esc falls through and closes the open cheat sheet', () => {
    const { win, modal } = setupWithPaletteAndModal(false);
    assert.ok(!modal.classList.contains('hidden'), 'precondition: cheat sheet must start open');
    fire(win, 'Escape');
    assert.ok(modal.classList.contains('hidden'),
      'Esc must close the cheat sheet when the palette is closed (fallthrough restored)');
  });

  it('palette OPEN (isOpen() true): Esc is claimed by the palette and the cheat sheet stays open', () => {
    const { win, modal, hideCalls } = setupWithPaletteAndModal(true);
    assert.ok(!modal.classList.contains('hidden'), 'precondition: cheat sheet must start open');
    fire(win, 'Escape');
    assert.equal(hideCalls.length, 1, 'swCommandPalette.hide() must be called when the palette is open');
    assert.ok(!modal.classList.contains('hidden'),
      'cheat sheet must remain open -- an open palette still wins Escape precedence');
  });
});

describe('keyboard.js: "s" focuses the secondary search (#1757 PR-4)', () => {
  it('s focuses the [data-sw-shortcut="s"] target when present', () => {
    const railHtml = '<input id="rep-rail-filter" type="search" data-sw-shortcut="s" data-sw-shortcut-label="Search reports">';
    const { win } = setup(railHtml);
    const evt = fire(win, 's');
    assert.equal(win.document.activeElement.id, 'rep-rail-filter', 's must focus the rail search input');
    assert.ok(evt.defaultPrevented, 's must preventDefault when it focuses its target');
  });

  it('s is a no-op (no throw, no focus steal) when no target exists', () => {
    const { win } = setup(); // base DOM has only the "/" search input
    const evt = fire(win, 's');
    assert.notEqual(win.document.activeElement.id, 'search', 's must not focus the "/" search box');
    assert.equal(evt.defaultPrevented, false, 's must not preventDefault without a target');
  });

  it('s while typing in an input does not steal focus', () => {
    const railHtml = '<input id="rep-rail-filter" type="search" data-sw-shortcut="s" data-sw-shortcut-label="Search reports">';
    const { win } = setup(railHtml);
    win.document.getElementById('search').focus();
    fire(win, 's');
    assert.equal(win.document.activeElement.id, 'search', 'typing "s" in a field must not move focus');
  });
});
