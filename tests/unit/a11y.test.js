// a11y.test.js - Structural accessibility scan for key interactive components.
//
// Uses axe-core against jsdom-rendered HTML fixtures to catch structural
// violations (missing labels, invalid ARIA attributes, role violations,
// button-name gaps) in the existing `make test-js` flow.
//
// Scope / limitations:
//   - jsdom has no CSS cascade: color-contrast checks are DISABLED here.
//     Real contrast validation runs via the Playwright smoke tier (make test-a11y).
//   - Fixtures are derived from the live .templ sources; update them when the
//     corresponding template changes ARIA structure.
//
// Rules exercised: button-name, label, aria-allowed-attr, aria-required-attr,
//   aria-valid-attr, aria-valid-attr-value, role-img (subset of aria-* group),
//   duplicate-id, frame-title, landmark-one-main (where applicable).
//
// Rules explicitly disabled:
//   - color-contrast: no CSS cascade in jsdom (see Playwright tier).
//   - region: fixtures are components, not full pages; landmark rules are noise.

import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { JSDOM } from 'jsdom';
import axe from 'axe-core';

import {
  bulkBarFixture,
  artworkModalFixture,
  dashboardCardsFixture,
  prefsDrawerFixture,
} from './helpers/a11y-fixtures.js';

// ---------------------------------------------------------------------------
// A11y test helper: run axe against an HTML string, return violations.
// ---------------------------------------------------------------------------

/**
 * runAxe renders html in jsdom and runs axe-core with the given rule overrides.
 * Returns an array of axe violations (each with id, impact, nodes[]).
 *
 * axe-core reads the `window` and `document` globals when determining its
 * execution environment. We temporarily assign them to the jsdom window for
 * the duration of the scan, then restore the originals. Tests run sequentially
 * inside node:test so there is no concurrent-clobber risk.
 *
 * @param {string} html  Full HTML document string.
 * @param {object} [ruleOverrides]  axe rule config overrides; defaults disable
 *   rules that jsdom cannot evaluate (color-contrast, region).
 * @returns {Promise<import('axe-core').Result[]>}
 */
async function runAxe(html, ruleOverrides = {}) {
  const dom = new JSDOM(html, {
    pretendToBeVisual: true,
    url: 'http://localhost:1973/',
  });

  const win = dom.window;

  // Temporarily expose jsdom globals so axe-core can locate the environment.
  const prevWindow   = global.window;
  const prevDocument = global.document;
  global.window   = win;
  global.document = win.document;

  const rules = {
    // Suppress layout/paint-dependent checks - jsdom has no CSS engine.
    'color-contrast': { enabled: false },
    // Page-level rules that don't apply to component-level fixtures.
    'region': { enabled: false },
    'landmark-one-main': { enabled: false },
    // Fixtures are bare HTML documents without a <title>; page-level rule,
    // not a component concern.
    'document-title': { enabled: false },
    // Merge caller overrides.
    ...ruleOverrides,
  };

  try {
    return await new Promise((resolve, reject) => {
      // Pass the root element (not the document itself) so axe can walk up to
      // ownerDocument + defaultView when the Node.js global `window` is absent.
      axe.run(
        win.document.documentElement,
        {
          runOnly: {
            type: 'tag',
            // Run wcag2a + wcag2aa rule sets (includes aria-*, button-name,
            // label) plus best-practice for name/role/value patterns.
            values: ['wcag2a', 'wcag2aa', 'best-practice'],
          },
          rules,
        },
        (err, results) => {
          if (err) { reject(err); return; }
          resolve(results.violations);
        },
      );
    });
  } finally {
    // Restore originals (undefined if they were not set before).
    global.window   = prevWindow;
    global.document = prevDocument;
  }
}

// ---------------------------------------------------------------------------
// Helper: format violations as a readable assertion message.
// ---------------------------------------------------------------------------
function formatViolations(violations) {
  if (violations.length === 0) return '(none)';
  return violations.map(v =>
    `  [${v.impact}] ${v.id}: ${v.description}\n` +
    v.nodes.slice(0, 2).map(n => `    target: ${JSON.stringify(n.target)}`).join('\n'),
  ).join('\n');
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('a11y structural scan (jsdom + axe-core)', () => {
  it('bulk-action bar has no structural a11y violations', async () => {
    const violations = await runAxe(bulkBarFixture);
    assert.deepEqual(
      violations,
      [],
      `Bulk-bar violations:\n${formatViolations(violations)}`,
    );
  });

  it('artwork modal has no structural a11y violations', async () => {
    const violations = await runAxe(artworkModalFixture);
    assert.deepEqual(
      violations,
      [],
      `Artwork modal violations:\n${formatViolations(violations)}`,
    );
  });

  it('dashboard stat cards have no structural a11y violations', async () => {
    const violations = await runAxe(dashboardCardsFixture);
    assert.deepEqual(
      violations,
      [],
      `Dashboard cards violations:\n${formatViolations(violations)}`,
    );
  });

  it('prefs drawer has no structural a11y violations', async () => {
    const violations = await runAxe(prefsDrawerFixture);
    assert.deepEqual(
      violations,
      [],
      `Prefs drawer violations:\n${formatViolations(violations)}`,
    );
  });
});

// ---------------------------------------------------------------------------
// Self-test: a deliberately-broken bulk-bar button MUST be caught.
//
// This test proves the scan is wired correctly: if the bulk-bar apply button
// loses its accessible name, the `button-name` rule fires. CI failing here
// means the GOOD fixtures above have a gap, not that the components are bad.
//
// Note on color-contrast: jsdom has no CSS engine, so contrast violations
// are NOT caught here. The Playwright smoke tier (make test-a11y) exercises
// real computed styles and IS the gate for contrast failures (issue #1943
// acceptance criterion: a dark:text-gray-700 on dark bg fails that scan).
// ---------------------------------------------------------------------------

describe('a11y self-test: deliberate violation is detected', () => {
  it('bulk-bar with unlabelled button triggers button-name violation', async () => {
    // Strip every accessible name from the apply button.
    // The fixture has the button text on its own indented line, so we use a
    // regex to remove the whitespace-padded text node.
    const broken = bulkBarFixture
      .replace(/aria-label="Apply bulk action"/, '')
      .replace(/>\s*Apply\s*<\/button>/, '></button>');

    const violations = await runAxe(broken);
    const ids = violations.map(v => v.id);
    assert.ok(
      ids.includes('button-name'),
      `Expected a 'button-name' violation when button has no label; got: ${ids.join(', ') || '(none)'}`,
    );
  });
});
