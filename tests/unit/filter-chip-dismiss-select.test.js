// Behavioral test for the optional `select` on the shared filter-chip dismiss
// scripts (web/components/filter_flyout.templ, issue #3093).
//
// WHY THIS EXISTS AND WHY IT IS NOT A GO TEST
//
// The Go tests beside these scripts assert on the RENDERED TEXT: that the call
// site passes a given argument, and that the emitted script contains the guard.
// Both are necessary and neither is sufficient, because templ inlines a script
// body verbatim -- so a text assertion passes for any code that merely CONTAINS
// those characters. Measured against the first version of this work, two
// mutations survived the whole Go suite:
//
//   1. guard kept, body changed to `opts.select = selectSel || '#artist-content'`
//   2. guard kept, `else { opts.select = 'body' }` added
//
// Mutation 2 makes EVERY caller emit select:'body', which is the real blanking
// scenario, and the suite stayed green. The Go call-site test guards ARGUMENTS;
// the Go script-body test guards THE GUARD'S TEXT; neither guards BEHAVIOR.
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

/**
 * runDismiss executes one extracted dismiss function against a stubbed htmx and
 * returns the options object it passed to htmx.ajax.
 *
 * The stub records rather than acts, so the assertion can inspect the exact
 * object shape -- specifically whether `select` is a PRESENT KEY, which is the
 * property no string match can see.
 */
function runDismiss(fnSource, fnName, args) {
  const calls = [];
  const errors = [];
  const sandbox = {
    console: { error: (...a) => errors.push(a.join(' ')) },
    history: { pushState() {} },
    document: { querySelector: () => null },
    URL,
    URLSearchParams,
  };
  sandbox.window = sandbox;
  sandbox.window.location = { href: 'http://localhost/reports/blast-radius?class=blanked&page=2' };
  sandbox.htmx = { ajax: (method, url, opts) => { calls.push({ method, url, opts }); } };

  vm.createContext(sandbox);
  vm.runInContext(`${fnSource}\nvar __result = ${fnName}(${args.map(a => JSON.stringify(a)).join(', ')});`, sandbox);
  return { calls, errors };
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
  // this tier happily tests the previous version and goes green. Measured --
  // applying a real mutation to the .templ without regenerating left both
  // suites passing.
  //
  // CHECKED BY CONTENT, NOT MTIME. An mtime comparison is the obvious form and
  // it is wrong twice over: `templ generate` does not rewrite an output whose
  // content is unchanged, so an ordinary `git checkout` or `cp` of the source
  // leaves it NEWER than a perfectly current artifact and the check fires on a
  // correct tree. (Measured while writing this: source 01:40:03, generated
  // 01:39:15, content identical and correct.) A false alarm that a regenerate
  // cannot clear is worse than no check, because the way out is to delete it.
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
    // the last statement untouched and slips straight past. Measured: a
    // last-statement marker reported 8/8 green against exactly that stale tree.
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
    const { calls } = runDismiss(chipFn, chipName, ['class', '#blast-radius-pane', '#blast-radius-pane']);
    assert.equal(calls.length, 1, 'the dismiss script did not call htmx.ajax exactly once');
    assert.equal(calls[0].opts.select, '#blast-radius-pane',
      'a supplied select selector did not reach the htmx options object');
  });

  it('drops ?page so a reload cannot land on a page the narrowed result set no longer has', () => {
    const { calls } = runDismiss(chipFn, chipName, ['class', '#compliance-results', '']);
    assert.ok(!calls[0].url.includes('page='), `?page survived into the reload URL: ${calls[0].url}`);
    assert.ok(!calls[0].url.includes('class='), `the dismissed key survived into the reload URL: ${calls[0].url}`);
  });

  it('fails loudly rather than silently when htmx is absent', () => {
    // The scripts are wired to inline onclick handlers, so a bare reference to
    // a missing global throws a ReferenceError with no indication of the cause
    // and the chip simply does nothing. The repo forbids that silent no-op.
    const calls = [];
    const errors = [];
    const sandbox = {
      console: { error: (...a) => errors.push(a.join(' ')) },
      history: { pushState() {} },
      document: { querySelector: () => null },
      URL, URLSearchParams,
    };
    sandbox.window = sandbox;
    sandbox.window.location = { href: 'http://localhost/reports/blast-radius?class=blanked' };
    // No htmx on the sandbox at all.
    vm.createContext(sandbox);
    assert.doesNotThrow(
      () => vm.runInContext(`${chipFn}\n${chipName}('class', '#compliance-results', '');`, sandbox),
      'the dismiss script threw when htmx was missing instead of reporting it',
    );
    assert.equal(calls.length, 0);
    assert.equal(errors.length, 1, 'no console.error was emitted for the missing htmx dependency');
    assert.ok(/htmx/i.test(errors[0]), `the error does not name the missing dependency: ${errors[0]}`);
  });
});

describe('DismissFilterValueChip select option', () => {
  // The multi-value branch of FilterChip2 routes here. It previously had no
  // select parameter at all, so a FilterChipSpec setting both Value and
  // SelectSel rendered a working chip whose dismiss did a bare full-page swap
  // -- the field was accepted, ignored and dropped with no error.
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
    const { calls } = runDismiss(valueChipFn, valueChipName, ['severity', '+error', '#blast-radius-pane', '#blast-radius-pane']);
    assert.equal(calls.length, 1);
    assert.equal(
      calls[0].opts.select, '#blast-radius-pane',
      'the value-chip dismiss dropped the caller\'s SelectSel. The chip renders and dismisses normally, so '
      + 'nothing fails visibly, but the swap takes the whole response body -- reintroducing exactly the '
      + 'defect the parameter exists to prevent, on the branch nobody looks at.',
    );
  });

  it('fails loudly rather than silently when htmx is absent, BEFORE calling ajax', () => {
    // The F7 guard on this script was entirely unproven: deleting it outright
    // left every suite in the repo green. Its sibling has this coverage; the
    // branch that needed the select-forwarding fix had an identical hole one
    // line below it.
    //
    // POSITION IS ASSERTED, NOT JUST PRESENCE. A guard that sits AFTER
    // htmx.ajax is a live failure mode, not a cosmetic one: the ReferenceError
    // fires first and the console.error never runs, so the operator gets the
    // silent no-op the guard exists to prevent while the code still LOOKS
    // guarded. Asserting only that some console.error exists would pass that.
    const calls = [];
    const errors = [];
    const sandbox = {
      console: { error: (...a) => errors.push(a.join(' ')) },
      history: { pushState() {} },
      document: { querySelector: () => null },
      URL, URLSearchParams,
    };
    sandbox.window = sandbox;
    sandbox.window.location = { href: 'http://localhost/dash?severity=%2Berror' };
    // No htmx on the sandbox at all.
    vm.createContext(sandbox);
    assert.doesNotThrow(
      () => vm.runInContext(`${valueChipFn}\n${valueChipName}('severity', '+error', '#action-queue', '');`, sandbox),
      'the value-chip dismiss threw when htmx was missing instead of reporting it',
    );
    assert.equal(calls.length, 0);
    assert.equal(errors.length, 1,
      'no console.error was emitted for the missing htmx dependency; the chip fails silently');
    assert.ok(/htmx/i.test(errors[0]), `the error does not name the missing dependency: ${errors[0]}`);

    // Position: the guard must precede the ajax call. Executed rather than
    // pattern-matched -- with htmx PRESENT but throwing, a guard placed after
    // the call would let the throw escape.
    const late = [];
    const s2 = {
      console: { error: (...a) => late.push('ERR:' + a.join(' ')) },
      history: { pushState() {} },
      document: { querySelector: () => null },
      URL, URLSearchParams,
    };
    s2.window = s2;
    s2.window.location = { href: 'http://localhost/dash?severity=%2Berror' };
    let ajaxRan = false;
    s2.htmx = { ajax: () => { ajaxRan = true; } };
    vm.createContext(s2);
    vm.runInContext(`${valueChipFn}\n${valueChipName}('severity', '+error', '#action-queue', '');`, s2);
    assert.equal(ajaxRan, true, 'with htmx present the script must reach htmx.ajax');
    assert.equal(late.length, 0, 'the guard reported an error while htmx was present');
  });

  it('keeps sibling values under the same key and removes only the dismissed one', () => {
    const calls = [];
    const sandbox = {
      console: { error() {} }, history: { pushState() {} },
      document: { querySelector: () => null }, URL, URLSearchParams,
    };
    sandbox.window = sandbox;
    sandbox.window.location = { href: 'http://localhost/dash?severity=%2Berror&severity=-info&page=3' };
    sandbox.htmx = { ajax: (m, u, o) => calls.push({ m, u, o }) };
    vm.createContext(sandbox);
    vm.runInContext(`${valueChipFn}\n${valueChipName}('severity', '+error', '#action-queue', '');`, sandbox);

    assert.equal(calls.length, 1);
    const q = new URLSearchParams(calls[0].u.split('?')[1] || '');
    const remaining = q.getAll('severity');
    // Precondition: the fixture really did carry two values, or "one survived"
    // is satisfied by a URL that only ever had one.
    assert.ok(!calls[0].u.includes('%2Berror') && !calls[0].u.includes('+error'),
      `the dismissed value survived: ${calls[0].u}`);
    assert.deepEqual(remaining, ['-info'],
      `sibling values under the same key were not preserved: ${JSON.stringify(remaining)}`);
  });
});
