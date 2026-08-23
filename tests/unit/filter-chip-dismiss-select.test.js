// Behavioral test for the optional `select` on the shared filter-chip dismiss
// scripts (web/components/filter_flyout.templ, issue #3093).
//
// WHY THIS EXISTS AND WHY IT IS NOT A GO TEST
//
// The Go tests beside these scripts assert on the RENDERED TEXT: that the call
// site passes a given argument, and that the emitted script contains the guard.
// Both are necessary and neither is sufficient, because templ inlines a script
// body verbatim -- so a text assertion passes for any code that merely CONTAINS
// those characters. Two mutations survive the whole Go suite:
//
//   1. guard kept, body changed to `opts.select = selectSel || '#artist-content'`
//   2. guard kept, `else { opts.select = 'body' }` added
//
// Mutation 2 makes EVERY caller emit select:'body', which is the real blanking
// scenario, and the Go suite stays green. The Go call-site test guards
// ARGUMENTS; the Go script-body test guards THE GUARD'S TEXT; neither guards
// BEHAVIOR.
//
// This test guards behavior: it EXECUTES the real emitted function against a
// stubbed htmx and inspects the options object that reaches htmx.ajax.
//
// HOW THIS TIER GETS RUN, which is part of its correctness.
//
// It is not invoked by scripts/pre-push-gate.sh; CI's `js-test` job runs it,
// and that job fires on a PATH FILTER (see the `js` filter in
// .github/workflows/ci.yml). The code under test lives in
// web/components/filter_flyout.templ, so that path is in the filter
// deliberately -- without it, editing only the .templ leaves this tier SKIPPED
// and the regression it exists to catch ships green. If this file moves, move
// the filter with it.
//
// THE SCRIPT IS EXTRACTED FROM THE GENERATED OUTPUT, NEVER RETYPED. A copy
// pasted into this file would drift from production the moment someone edits
// the .templ, and the test would then prove a property of the copy. Reading
// filter_flyout_templ.go means the bytes under test are the bytes that ship.

import { describe, it, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const dirname = path.dirname(fileURLToPath(import.meta.url));
const GENERATED = path.join(dirname, '..', '..', 'web', 'components', 'filter_flyout_templ.go');
const SOURCE = path.join(dirname, '..', '..', 'web', 'components', 'filter_flyout.templ');

/**
 * extractScript pulls one generated `function __templ_<Name>_<hash>(...) {...}`
 * body out of the templ-generated Go file.
 *
 * Brace-counting rather than a lazy regex: the script bodies contain nested
 * braces (object literals, if blocks, callbacks), so a non-greedy match to the
 * first `}` truncates mid-function and the vm would fail to parse for a reason
 * that has nothing to do with the property under test.
 *
 * KNOWN LIMIT: the counter does not skip comments or string literals, so a `{`
 * or `}` appearing in comment prose will unbalance it and truncate the
 * extraction. That is a plausible edit here, since the comments in these
 * scripts already quote htmx internals like `if(i){d=i}`. It fails loudly (the
 * vm raises a SyntaxError) rather than silently testing a fragment, but the
 * error points at the parse rather than at the real cause, so it is named here.
 */
function extractScript(src, name) {
  const startRe = new RegExp(`function __templ_${name}_[0-9a-f]+\\([^)]*\\)\\{`);
  const m = src.match(startRe);
  if (!m) throw new Error(`generated function __templ_${name}_* not found in ${GENERATED}`);
  const open = m.index + m[0].length - 1;
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') {
      depth--;
      if (depth === 0) return src.slice(m.index, i + 1);
    }
  }
  throw new Error(`unbalanced braces while extracting __templ_${name}_*`);
}

const DEFAULT_HREF = 'http://localhost/reports/example?class=blanked&page=2';

/**
 * makeSandbox builds the vm context these scripts run in, and RECORDS every
 * outside effect they can have: htmx.ajax calls, history.pushState calls, and
 * console.error messages. One builder rather than a literal per test, so a new
 * recorder reaches every case at once.
 *
 * `htmx` selects which of two shapes the context has:
 *
 *   'present' (default) -- the browser shape. `window` IS the global object, so
 *   `window.htmx` and a bare `htmx` are the same recording stub.
 *
 *   'missing' -- `window` is a SEPARATE object carrying only `location`, so the
 *   scripts' `!window.htmx` predicate is true, while the recording stub stays
 *   reachable through the bare global as a TRIPWIRE. That split does not exist
 *   in a browser and is not meant to model one: it exists so that "the script
 *   did not reach htmx.ajax" is an assertion about a recorder that EXISTS. With
 *   no stub anywhere, `calls.length === 0` holds for any code at all, which is
 *   what made the original version of this assertion vacuous.
 *
 * `metaBasePath`, when set, makes `document.querySelector('meta[name="htmx-base-path"]')`
 * return an object carrying that `content` -- the real DOM shape the scripts
 * read `bp` from. Omitted (the default) keeps `querySelector` returning null,
 * i.e. bp === '' throughout, matching every existing case in this file. This
 * is what lets #3094 FIX 4's tests put a NON-EMPTY bp into the sandbox at all;
 * without it every case here has bp === '' and the strip's `if (bp && ...)`
 * guard, let alone its `else if (bp)` branch, can never engage.
 */
function makeSandbox({ href = DEFAULT_HREF, htmx = 'present', metaBasePath = null } = {}) {
  const calls = [];
  const errors = [];
  const pushes = [];
  const stub = { ajax: (method, url, opts) => { calls.push({ method, url, opts }); } };
  const sandbox = {
    console: { error: (...a) => errors.push(a.join(' ')) },
    history: { pushState: (...a) => { pushes.push(a); } },
    document: {
      querySelector: (sel) => (
        metaBasePath !== null && sel === 'meta[name="htmx-base-path"]'
          ? { content: metaBasePath }
          : null
      ),
    },
    URL,
    URLSearchParams,
    htmx: stub,
  };
  if (htmx === 'missing') {
    sandbox.window = { location: { href } };
  } else {
    sandbox.window = sandbox;
    sandbox.window.location = { href };
  }
  vm.createContext(sandbox);
  return { sandbox, calls, errors, pushes };
}

/**
 * runDismiss executes one extracted dismiss function in a fresh sandbox and
 * returns everything that sandbox recorded.
 *
 * The stub records rather than acts, so the assertion can inspect the exact
 * object shape -- specifically whether `select` is a PRESENT KEY, which is the
 * property no string match can see.
 */
function runDismiss(fnSource, fnName, args, opts = {}) {
  const { sandbox, calls, errors, pushes } = makeSandbox(opts);
  vm.runInContext(`${fnSource}\nvar __result = ${fnName}(${args.map(a => JSON.stringify(a)).join(', ')});`, sandbox);
  return { calls, errors, pushes };
}

let source;
let chipFn;
let valueChipFn;
let chipName;
let valueChipName;

before(() => {
  // FRESHNESS, because the extraction has a mirror blind spot.
  //
  // Reading the generated file rather than a pasted copy is what lets this tier
  // claim the bytes under test are the bytes that ship. That claim is FALSE
  // against an un-regenerated tree: edit the .templ, skip `templ generate`, and
  // this tier happily tests the previous version and goes green: applying a real
  // mutation to the .templ without regenerating leaves both suites passing.
  //
  // CHECKED BY CONTENT, NOT MTIME. An mtime comparison is the obvious form and
  // it is wrong twice over: `templ generate` does not rewrite an output whose
  // content is unchanged, so an ordinary `git checkout` or `cp` of the source
  // leaves it NEWER than a perfectly current artifact and the check fires on a
  // correct tree. A false alarm that a regenerate cannot clear is worse than no
  // check, because the way out is to delete it.
  //
  // So: extract each script body from BOTH files and compare. templ inlines the
  // body verbatim, so the generated copy must contain the source's, modulo the
  // leading tabs templ strips. This is true exactly when the artifact is
  // current, needs no filesystem timing, and cannot be cleared by touching a
  // file.
  //
  // scripts/check-generated.sh also catches staleness in the pre-push gate; this
  // makes the tier self-contained rather than borrowing that guarantee.
  const srcText = fs.readFileSync(SOURCE, 'utf8');
  const genText = fs.readFileSync(GENERATED, 'utf8');
  for (const name of ['DismissFilterChip', 'DismissFilterValueChip']) {
    const srcRe = new RegExp(`script ${name}\\([^)]*\\)\\s*\\{([\\s\\S]*?)\\n\\}`, 'm');
    const m = srcText.match(srcRe);
    assert.ok(m, `could not find the \`script ${name}\` block in ${SOURCE}`);

    // EVERY code line must be present in the generated copy, not just one
    // marker. A single sentinel (the last statement, say) only detects edits
    // that happen to touch that line: the mutation this tier exists to catch --
    // adding `else { opts.select = 'body' }` in the middle of the body -- leaves
    // the last statement untouched and slips straight past: a last-statement
    // marker reports green against exactly that stale tree.
    //
    // Comments are skipped because templ ships them verbatim but they carry no
    // behavior, and blank lines because indentation is normalized.
    const codeLines = m[1]
      .split('\n')
      .map(l => l.trim())
      .filter(l => l && !l.startsWith('//'));
    assert.ok(codeLines.length > 0, `the \`script ${name}\` block in ${SOURCE} has no code lines`);

    const missing = codeLines.filter(l => !genText.includes(l));
    assert.deepEqual(
      missing, [],
      `${GENERATED} is missing ${missing.length} code line(s) that ${SOURCE} defines for ${name}, e.g. `
      + `${JSON.stringify(missing[0])}. The generated artifact is STALE, so this tier would extract and test `
      + 'the previous version of the code and report green. Run `go tool templ generate`.',
    );
  }

  source = fs.readFileSync(GENERATED, 'utf8');
  chipFn = extractScript(source, 'DismissFilterChip');
  valueChipFn = extractScript(source, 'DismissFilterValueChip');
  chipName = chipFn.match(/function (__templ_DismissFilterChip_[0-9a-f]+)/)[1];
  valueChipName = valueChipFn.match(/function (__templ_DismissFilterValueChip_[0-9a-f]+)/)[1];
});

describe('DismissFilterChip select option', () => {
  it('omits the select KEY entirely when no caller asked for one', () => {
    const { calls } = runDismiss(chipFn, chipName, ['class', '#compliance-results', '']);

    // Precondition: the function actually reached htmx.ajax. Without this a
    // script that returned early would satisfy every assertion below by
    // producing no options object to object to.
    assert.equal(calls.length, 1, 'the dismiss script did not call htmx.ajax exactly once');

    const { opts } = calls[0];
    assert.equal(opts.target, '#compliance-results', 'the caller-supplied target was not used');
    assert.equal(
      Object.prototype.hasOwnProperty.call(opts, 'select'), false,
      'a caller that supplied no select still got a `select` key in its htmx options. Whatever value it '
      + 'holds, the key is now part of this caller\'s swap contract, and any non-empty value changes what '
      + 'htmx extracts from the response for a caller that never asked. Got: ' + JSON.stringify(opts),
    );
  });

  it('passes the caller\'s selector through when one IS supplied', () => {
    const { calls } = runDismiss(chipFn, chipName, ['class', '#report-pane', '#report-pane']);
    assert.equal(calls.length, 1, 'the dismiss script did not call htmx.ajax exactly once');
    assert.equal(calls[0].opts.select, '#report-pane',
      'a supplied select selector did not reach the htmx options object');
  });

  it('drops ?page so a reload cannot land on a page the narrowed result set no longer has', () => {
    const { calls } = runDismiss(chipFn, chipName, ['class', '#compliance-results', '']);
    assert.ok(!calls[0].url.includes('page='), `?page survived into the reload URL: ${calls[0].url}`);
    assert.ok(!calls[0].url.includes('class='), `the dismissed key survived into the reload URL: ${calls[0].url}`);
  });

  it('fails loudly AND without side effects when htmx is absent', () => {
    // The scripts are wired to inline onclick handlers, so a bare reference to
    // a missing global throws a ReferenceError with no indication of the cause
    // and the chip simply does nothing. The repo forbids that silent no-op.
    //
    // FAILING LOUDLY AND FAILING WITHOUT SIDE EFFECTS ARE DIFFERENT PROPERTIES
    // and this asserts both. A guard that sits below the URL surgery reports the
    // missing dependency correctly and still leaves history.pushState having
    // stripped the filter from the address bar with nothing reloaded: the chip
    // stays on screen, URL and rendered content disagree, and a later manual
    // refresh applies a filter state the operator never saw applied. The
    // pushes assertion below is what catches that ordering; the errors
    // assertion alone passes for it.
    const { sandbox, calls, errors, pushes } = makeSandbox({
      href: 'http://localhost/reports/example?class=blanked',
      htmx: 'missing',
    });
    assert.doesNotThrow(
      () => vm.runInContext(`${chipFn}\n${chipName}('class', '#compliance-results', '');`, sandbox),
      'the dismiss script threw when htmx was missing instead of reporting it',
    );
    assert.deepEqual(
      pushes, [],
      'the dismiss script rewrote the address bar before discovering htmx was missing, so the filter is gone '
      + 'from the URL while the chip and the rendered rows still show it applied. The guard must run before '
      + `any URL mutation. history.pushState arguments: ${JSON.stringify(pushes)}`,
    );
    assert.equal(
      calls.length, 0,
      'the dismiss script reached htmx.ajax despite window.htmx being absent; the guard did not return',
    );
    assert.equal(errors.length, 1, 'no console.error was emitted for the missing htmx dependency');
    assert.ok(/htmx/i.test(errors[0]), `the error does not name the missing dependency: ${errors[0]}`);
    assert.ok(/#compliance-results/.test(errors[0]),
      `the error does not name the target that failed to reload: ${errors[0]}`);
  });

  it('reaches htmx.ajax and rewrites the URL on the ordinary path', () => {
    // The complement of the guard test: it proves the guard does not fire when
    // htmx IS present, so "no ajax, no pushState" above is a property of the
    // missing-dependency case rather than of the script always doing nothing.
    const { calls, errors, pushes } = runDismiss(chipFn, chipName, ['class', '#compliance-results', '']);
    assert.equal(calls.length, 1, 'with htmx present the script must reach htmx.ajax');
    assert.equal(pushes.length, 1, 'with htmx present the script must rewrite the address bar');
    assert.deepEqual(errors, [], `the guard reported an error while htmx was present: ${JSON.stringify(errors)}`);
  });

  // #3094 FIX 4: the strip's `if (bp && path.startsWith(bp))` had no `else`,
  // so a non-empty bp that does NOT prefix location.pathname silently fell
  // through and issued a double-prefixed request with no error anywhere.
  it('fails loudly when a non-empty base path does not prefix the current location (#3094 FIX 4)', () => {
    const { calls, errors } = runDismiss(chipFn, chipName, ['class', '#compliance-results', ''], {
      href: 'http://localhost/reports/example?class=blanked&page=2',
      metaBasePath: '/sw',
    });
    // Precondition: the fixture href genuinely does NOT start with the
    // configured base path, or the mismatch this test exists to catch would
    // not be present in the first place.
    assert.ok(
      !new URL('http://localhost/reports/example').pathname.startsWith('/sw'),
      'fixture href already starts with the base path; the mismatch case is not actually exercised',
    );
    assert.equal(errors.length, 1, 'no console.error was emitted for the base-path mismatch');
    assert.ok(/base path/i.test(errors[0]), `the error does not mention the base path: ${errors[0]}`);
    assert.ok(/\/sw/.test(errors[0]), `the error does not name the configured base path: ${errors[0]}`);
    // The mismatch is reported, not silently worked around: the request still
    // goes out (this script has no recovery path, only a louder failure), so
    // the assertion is on the ERROR being present, not on the request being
    // suppressed.
    assert.equal(calls.length, 1, 'the script did not reach htmx.ajax despite reporting the mismatch');
  });

  it('does not report a mismatch when the base path is empty (root deployment)', () => {
    // Complement of the above: proves the new else-branch is conditioned on
    // bp being non-empty, not on the mismatch alone -- an empty bp (no
    // sub-path deployment) must stay silent, matching every pre-#3094 case.
    const { errors } = runDismiss(chipFn, chipName, ['class', '#compliance-results', ''], {
      href: 'http://localhost/reports/example?class=blanked&page=2',
      metaBasePath: '',
    });
    assert.deepEqual(errors, [], `an empty base path must not report a mismatch: ${JSON.stringify(errors)}`);
  });

  it('does not report a mismatch when the base path DOES prefix the location', () => {
    // Complement of the mismatch case: the ordinary sub-path-deployment path
    // (bp non-empty AND matching) must stay silent -- this is the SAME
    // scenario proved correct end-to-end in
    // tests/a11y/base-path-filter-idiom.spec.js, at the unit-test layer.
    const { errors } = runDismiss(chipFn, chipName, ['class', '#compliance-results', ''], {
      href: 'http://localhost/sw/reports/example?class=blanked&page=2',
      metaBasePath: '/sw',
    });
    assert.deepEqual(errors, [], `a matching base path must not report a mismatch: ${JSON.stringify(errors)}`);
  });
});

describe('DismissFilterValueChip select option', () => {
  // The multi-value branch of FilterChip2 routes here. Without a select
  // parameter of its own, a FilterChipSpec setting both Value and SelectSel
  // renders a working chip whose dismiss does a bare full-page swap -- the
  // field accepted, ignored and dropped with no error.
  it('omits the select KEY entirely when no caller asked for one', () => {
    const { calls } = runDismiss(valueChipFn, valueChipName, ['severity', '+error', '#action-queue', '']);
    assert.equal(calls.length, 1, 'the value-chip dismiss script did not call htmx.ajax exactly once');
    assert.equal(
      Object.prototype.hasOwnProperty.call(calls[0].opts, 'select'), false,
      'the value-chip dismiss emitted a `select` key for a caller that supplied none: '
      + JSON.stringify(calls[0].opts),
    );
  });

  it('carries a supplied selector through, rather than silently dropping it', () => {
    const { calls } = runDismiss(valueChipFn, valueChipName, ['severity', '+error', '#report-pane', '#report-pane']);
    assert.equal(calls.length, 1);
    assert.equal(
      calls[0].opts.select, '#report-pane',
      'the value-chip dismiss dropped the caller\'s SelectSel. The chip renders and dismisses normally, so '
      + 'nothing fails visibly, but the swap takes the whole response body -- reintroducing exactly the '
      + 'defect the parameter exists to prevent, on the branch that is easiest to miss.',
    );
  });

  it('fails loudly rather than silently when htmx is absent, BEFORE calling ajax', () => {
    // POSITION IS ASSERTED, NOT JUST PRESENCE. A guard that sits AFTER
    // htmx.ajax is a live failure mode, not a cosmetic one: the ReferenceError
    // fires first and the console.error never runs, so the operator gets the
    // silent no-op the guard exists to prevent while the code still LOOKS
    // guarded. Asserting only that some console.error exists would pass that.
    const { sandbox, calls, errors, pushes } = makeSandbox({
      href: 'http://localhost/dash?severity=%2Berror',
      htmx: 'missing',
    });
    assert.doesNotThrow(
      () => vm.runInContext(`${valueChipFn}\n${valueChipName}('severity', '+error', '#action-queue', '');`, sandbox),
      'the value-chip dismiss threw when htmx was missing instead of reporting it',
    );
    assert.deepEqual(
      pushes, [],
      'the value-chip dismiss rewrote the address bar before discovering htmx was missing, dropping the value '
      + 'from the URL while the chip still shows it applied and nothing reloaded. The guard must run before any '
      + `URL mutation. history.pushState arguments: ${JSON.stringify(pushes)}`,
    );
    assert.equal(
      calls.length, 0,
      'the value-chip dismiss reached htmx.ajax despite window.htmx being absent; the guard did not return',
    );
    assert.equal(errors.length, 1,
      'no console.error was emitted for the missing htmx dependency; the chip fails silently');
    assert.ok(/htmx/i.test(errors[0]), `the error does not name the missing dependency: ${errors[0]}`);
    assert.ok(/#action-queue/.test(errors[0]),
      `the error does not name the target that failed to reload: ${errors[0]}`);

    // Position: the guard must precede the ajax call. Executed rather than
    // pattern-matched -- with htmx PRESENT the script must run all the way
    // through without the guard firing.
    const present = runDismiss(valueChipFn, valueChipName, ['severity', '+error', '#action-queue', ''], {
      href: 'http://localhost/dash?severity=%2Berror',
    });
    assert.equal(present.calls.length, 1, 'with htmx present the script must reach htmx.ajax');
    assert.equal(present.pushes.length, 1, 'with htmx present the script must rewrite the address bar');
    assert.deepEqual(present.errors, [],
      `the guard reported an error while htmx was present: ${JSON.stringify(present.errors)}`);
  });

  it('keeps sibling values under the same key and removes only the dismissed one', () => {
    const { calls } = runDismiss(
      valueChipFn, valueChipName, ['severity', '+error', '#action-queue', ''],
      { href: 'http://localhost/dash?severity=%2Berror&severity=-info&page=3' },
    );

    assert.equal(calls.length, 1);
    const q = new URLSearchParams(calls[0].url.split('?')[1] || '');
    const remaining = q.getAll('severity');
    // Precondition: the fixture really did carry two values, or "one survived"
    // is satisfied by a URL that only ever had one.
    assert.ok(!calls[0].url.includes('%2Berror') && !calls[0].url.includes('+error'),
      `the dismissed value survived: ${calls[0].url}`);
    assert.deepEqual(remaining, ['-info'],
      `sibling values under the same key were not preserved: ${JSON.stringify(remaining)}`);
  });

  // #3094 FIX 4: same missing-else defect as DismissFilterChip, same fix,
  // same tests.
  it('fails loudly when a non-empty base path does not prefix the current location (#3094 FIX 4)', () => {
    const { calls, errors } = runDismiss(
      valueChipFn, valueChipName, ['severity', '+error', '#action-queue', ''],
      { href: 'http://localhost/dash?severity=%2Berror', metaBasePath: '/sw' },
    );
    assert.ok(
      !new URL('http://localhost/dash').pathname.startsWith('/sw'),
      'fixture href already starts with the base path; the mismatch case is not actually exercised',
    );
    assert.equal(errors.length, 1, 'no console.error was emitted for the base-path mismatch');
    assert.ok(/base path/i.test(errors[0]), `the error does not mention the base path: ${errors[0]}`);
    assert.ok(/\/sw/.test(errors[0]), `the error does not name the configured base path: ${errors[0]}`);
    assert.equal(calls.length, 1, 'the script did not reach htmx.ajax despite reporting the mismatch');
  });

  it('does not report a mismatch when the base path is empty (root deployment)', () => {
    const { errors } = runDismiss(
      valueChipFn, valueChipName, ['severity', '+error', '#action-queue', ''],
      { href: 'http://localhost/dash?severity=%2Berror', metaBasePath: '' },
    );
    assert.deepEqual(errors, [], `an empty base path must not report a mismatch: ${JSON.stringify(errors)}`);
  });

  it('does not report a mismatch when the base path DOES prefix the location', () => {
    const { errors } = runDismiss(
      valueChipFn, valueChipName, ['severity', '+error', '#action-queue', ''],
      { href: 'http://localhost/sw/dash?severity=%2Berror', metaBasePath: '/sw' },
    );
    assert.deepEqual(errors, [], `a matching base path must not report a mismatch: ${JSON.stringify(errors)}`);
  });
});
