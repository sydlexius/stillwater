// Behavioral test for blastRadiusApplyOrdering (#3101), the handler wired to
// the blast-radius pane's sort/order <select> onchange (web/templates/
// reports_page.templ).
//
// WHY THIS EXISTS AND WHY IT IS NOT A GO TEST
//
// blastRadiusApplyOrdering is plain browser JS, embedded verbatim in a
// templ <script> block rather than a templ.ComponentScript -- so it is
// reachable by neither the Go table tests beside the handler (which assert on
// the server-rendered SELECT markup and the ROWS a query returns, never on
// this client-side URL rewrite) nor by a Playwright a11y spec scoped to a
// route the ordering controls fire against (that tier already covers the
// serialization race, but never asserted the ?page reset itself). This is the
// gap #3101 names: "a grep for blastRadiusApplyOrdering across tests/ returns
// nothing."
//
// LAYER CHOSEN: a jsdom/vm unit spec extracting the function verbatim from the
// generated reports_page_templ.go, executing it against a stubbed
// document/URL/history, and asserting on the resulting query string. This is
// the layer the issue's "What to do" section asks for directly, and it is the
// cheapest layer that can observe the property at all: the property is a pure
// string transform (drop `page`, set `sort`/`order`, leave everything else),
// which needs no rendered DOM, no HTTP round trip, and no browser engine to
// verify -- an a11y-tier spec would pay a full Playwright+server round trip to
// assert exactly the same query-string diff this tier reads directly. A
// second, redundant a11y spec was considered and rejected: the a11y tier
// already covers "sort a11y describes IS observably applied" via the ordering
// race tests, so a browser-level page-reset spec would duplicate a boundary a
// cheaper tier already pins, for no additional confidence.
//
// THE SCRIPT IS EXTRACTED FROM THE GENERATED OUTPUT, NEVER RETYPED. A copy
// pasted into this file would drift from production the moment someone edits
// the .templ, and the test would then prove a property of the copy. Reading
// reports_page_templ.go means the bytes under test are the bytes that ship.
//
// UNLIKE filter-chip-dismiss-select.test.js, this function is NOT a templ
// `script` directive (no `templ.ComponentScript`, no generated hashed
// `__templ_Name_<hash>` wrapper) -- it is a plain `function
// blastRadiusApplyOrdering() { ... }` embedded in an ordinary <script> block
// alongside several others. So the extraction here is a brace-counting scan
// for that literal signature, not a templ-specific name/hash regex.

import { describe, it, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const dirname = path.dirname(fileURLToPath(import.meta.url));
const GENERATED = path.join(dirname, '..', '..', 'web', 'templates', 'reports_page_templ.go');
const SOURCE = path.join(dirname, '..', '..', 'web', 'templates', 'reports_page.templ');

/**
 * unescapeGoString decodes the small set of escapes templ actually emits in
 * its generated `templruntime.WriteString(..., "...")` calls (\n, \t, \r, \",
 * \\). This function is a plain `function name() {...}` inside such a
 * double-quoted Go string literal, NOT a templ `script` directive -- those
 * (see filter_flyout_templ.go's `Function: \`...\`` fields) are emitted as Go
 * RAW string literals (backtick-quoted), which need no decoding at all. That
 * is why filter-chip-dismiss-select.test.js's extractScript has no unescape
 * step and this one does: same idea, different generated shape.
 */
function unescapeGoString(s) {
  return s.replace(/\\(n|t|r|"|\\)/g, (_, c) => ({ n: '\n', t: '\t', r: '\r', '"': '"', '\\': '\\' })[c]);
}

/**
 * extractFunction pulls one `function <name>(...) {...}` body out of the
 * escaped Go source string, decoding as it scans. Brace-counting rather than
 * a lazy regex, for the same reason extractScript in
 * filter-chip-dismiss-select.test.js counts braces: the body nests nested
 * braces (an if-block, in this case), and a non-greedy match to the first
 * `}` would truncate mid-function. Depth-counting runs over the DECODED
 * braces so an escaped `\{` in a comment (none exist here, but the extractor
 * should not assume) cannot desync the count.
 */
function extractFunction(src, name) {
  const decoded = unescapeGoString(src);
  const startRe = new RegExp(`function ${name}\\(\\)\\s*\\{`);
  const m = decoded.match(startRe);
  if (!m) throw new Error(`function ${name} not found`);
  const open = m.index + m[0].length - 1;
  let depth = 0;
  for (let i = open; i < decoded.length; i++) {
    if (decoded[i] === '{') depth++;
    else if (decoded[i] === '}') {
      depth--;
      if (depth === 0) return decoded.slice(m.index, i + 1);
    }
  }
  throw new Error(`unbalanced braces while extracting ${name}`);
}

const DEFAULT_HREF = 'http://localhost/reports/blast-radius'
  + '?sort=artist_name&order=asc&page=3&page_size=10&class=blanked&attribution=automated'
  + '&field=biography&artist_id=f-art-1';

/**
 * makeSandbox builds a vm context with stub <select> elements for
 * #blast-radius-sort / #blast-radius-order (or omits them, to exercise the
 * capability guard), and records every history.pushState call and
 * console.error message.
 *
 * blastRadiusReload is stubbed as a no-op recorder: this test is about the URL
 * blastRadiusApplyOrdering builds, not about the reload it triggers (that is
 * the a11y tier's job, and is already covered there).
 */
function makeSandbox({
  href = DEFAULT_HREF,
  sortValue = 'field',
  orderValue = 'desc',
  omitSort = false,
  omitOrder = false,
} = {}) {
  const pushes = [];
  const errors = [];
  const reloads = [];
  const elements = {};
  if (!omitSort) elements['blast-radius-sort'] = { value: sortValue };
  if (!omitOrder) elements['blast-radius-order'] = { value: orderValue };

  const sandbox = {
    console: { error: (...a) => errors.push(a.join(' ')) },
    history: { pushState: (...a) => pushes.push(a) },
    document: { getElementById: (id) => elements[id] || null },
    URL,
    URLSearchParams,
    blastRadiusReload: () => reloads.push(true),
  };
  sandbox.window = sandbox;
  sandbox.window.location = { href };
  vm.createContext(sandbox);
  return { sandbox, pushes, errors, reloads };
}

function run(fnSource, opts = {}) {
  const { sandbox, pushes, errors, reloads } = makeSandbox(opts);
  vm.runInContext(`${fnSource}\nblastRadiusApplyOrdering();`, sandbox);
  return { pushes, errors, reloads };
}

let source;
let fnSource;

before(() => {
  // FRESHNESS, same reasoning and same mechanism as
  // filter-chip-dismiss-select.test.js: read both files, and fail loudly if
  // the generated artifact does not contain every code line the .templ
  // defines for this function, rather than silently testing a stale copy.
  const srcText = fs.readFileSync(SOURCE, 'utf8');
  const genText = fs.readFileSync(GENERATED, 'utf8');

  const srcRe = /function blastRadiusApplyOrdering\(\)\s*\{([\s\S]*?)\n\t\t\}/;
  const m = srcText.match(srcRe);
  assert.ok(m, `could not find function blastRadiusApplyOrdering in ${SOURCE}`);

  const codeLines = m[1]
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('//'));
  assert.ok(codeLines.length > 0, `blastRadiusApplyOrdering in ${SOURCE} has no code lines`);

  const missing = codeLines.filter((l) => !genText.includes(l));
  assert.deepEqual(
    missing, [],
    `${GENERATED} is missing ${missing.length} code line(s) that ${SOURCE} defines for `
    + `blastRadiusApplyOrdering, e.g. ${JSON.stringify(missing[0])}. The generated artifact is STALE, so this `
    + 'tier would extract and test the previous version of the code and report green. Run `go tool templ generate`.',
  );

  source = genText;
  fnSource = extractFunction(source, 'blastRadiusApplyOrdering');
});

describe('blastRadiusApplyOrdering', () => {
  it('removes ?page from the URL', () => {
    // Precondition: the fixture URL genuinely carries a page param, or "page
    // is gone" would be vacuously true.
    assert.ok(DEFAULT_HREF.includes('page=3'), 'fixture URL does not carry a page param to begin with');

    const { pushes } = run(fnSource);
    assert.equal(pushes.length, 1, 'blastRadiusApplyOrdering did not rewrite the address bar');
    const url = new URL(pushes[0][2]);
    assert.equal(
      url.searchParams.has('page'), false,
      `?page survived the ordering change: ${url.toString()}`,
    );
  });

  it('sets sort and order to the selects\' current values', () => {
    const { pushes } = run(fnSource, { sortValue: 'field', orderValue: 'desc' });
    const url = new URL(pushes[0][2]);
    assert.equal(url.searchParams.get('sort'), 'field');
    assert.equal(url.searchParams.get('order'), 'desc');
  });

  it('leaves every other axis untouched: class, attribution, field, artist_id, page_size all survive', () => {
    // Precondition: the fixture URL actually carries all five axes, or
    // "they survive" would be vacuously true for whichever ones were never
    // present.
    const before = new URL(DEFAULT_HREF);
    for (const axis of ['class', 'attribution', 'field', 'artist_id', 'page_size']) {
      assert.ok(before.searchParams.has(axis), `fixture URL does not carry ${axis}= to begin with`);
    }

    const { pushes } = run(fnSource);
    const after = new URL(pushes[0][2]);
    for (const axis of ['class', 'attribution', 'field', 'artist_id', 'page_size']) {
      assert.equal(
        after.searchParams.get(axis), before.searchParams.get(axis),
        `${axis} did not survive the ordering change: was ${before.searchParams.get(axis)}, `
        + `now ${after.searchParams.get(axis)}. This is the property that lets ordering compose with `
        + 'filtering; a re-sort must not silently clear or alter a narrowing axis.',
      );
    }
  });

  it('reloads the pane after rewriting the URL', () => {
    const { reloads } = run(fnSource);
    assert.equal(reloads.length, 1, 'blastRadiusApplyOrdering did not call blastRadiusReload');
  });

  it('fails loudly and without side effects when the sort control is missing', () => {
    // Matches the existing console.error the handler already emits when the
    // controls are absent -- the repo forbids a silent no-op here.
    const { pushes, errors, reloads } = run(fnSource, { omitSort: true });
    assert.deepEqual(pushes, [], 'the address bar was rewritten despite the sort control being missing');
    assert.deepEqual(reloads, [], 'the pane was reloaded despite the sort control being missing');
    assert.equal(errors.length, 1, 'no console.error was emitted for the missing sort control');
    assert.ok(/sort.*order|ordering/i.test(errors[0]), `the error does not name the missing control: ${errors[0]}`);
  });

  it('fails loudly and without side effects when the order control is missing', () => {
    const { pushes, errors, reloads } = run(fnSource, { omitOrder: true });
    assert.deepEqual(pushes, [], 'the address bar was rewritten despite the order control being missing');
    assert.deepEqual(reloads, [], 'the pane was reloaded despite the order control being missing');
    assert.equal(errors.length, 1, 'no console.error was emitted for the missing order control');
  });
});
