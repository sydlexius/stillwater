// Tests for the "also delete near-duplicate backdrops on media servers" switch
// in the duplicate-images rule's Configure form (#3138 S4a), as driven by
// web/static/js/settings/rule-toggle.js.
//
// The option turns on deletion of images on a remote media server, so the
// contract is about what is SENT and, above all, what is NOT: off -> on sends
// nothing until the confirm dialog is accepted, and so does on -> on; cancel, a
// missing dialog and a refused tolerance send nothing; on -> off and off -> off
// save straight through, with off OMITTING the key; the hidden tolerance always
// rides along.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

const RULE_TOGGLE_SRC = readFileSync(join(
  resolve(dirname(fileURLToPath(import.meta.url)), '../..'),
  'web/static/js/settings/rule-toggle.js',
), 'utf-8');

// The form markup settings.templ renders, reduced to what the script reads.
function formHtml({ stored, blocked = false, tolerance = '0.95' }) {
  const locked = blocked && !stored;
  return `<!doctype html><html><body>
<div id="rule-cfg-image_duplicate">
  <form data-rule-id="image_duplicate">
    ${tolerance ? `<input type="hidden" name="tolerance" value="${tolerance}">` : ''}
    <button type="button" role="switch" class="${stored ? 'BTN-ON' : 'BTN-OFF'}"
      aria-checked="${stored}" data-prune-platform-copies data-initial="${stored}"
      ${blocked ? 'data-prune-blocked' : ''} ${locked ? 'aria-disabled="true"' : ''}
      data-sw-btn-on="BTN-ON" data-sw-btn-off="BTN-OFF"
      data-sw-knob-on="KNOB-ON" data-sw-knob-off="KNOB-OFF"
      data-prune-confirm-title="TITLE" data-prune-confirm-body="BODY"
      data-prune-confirm-accept="ACCEPT"><span class="${stored ? 'KNOB-ON' : 'KNOB-OFF'}"></span></button>
    <select name="severity"><option value="warning" selected>warning</option><option value="error">error</option></select>
  </form>
</div></body></html>`;
}

// setup loads the module over the form. dialog 'record' captures the confirm
// call so a test can accept or walk away; 'missing' leaves the API undefined.
// server is what the GET before a stored-on save reports: 'on', 'off' or 'fail';
// rulesList overrides the rules it returns.
function setup({ stored, blocked, tolerance, dialog = 'record', response, server = 'on', rulesList } = {}) {
  const dom = createDom({ html: formHtml({ stored, blocked, tolerance }), csrfToken: 'tok' });
  const win = dom.window;
  const rules = rulesList || [{ id: 'image_duplicate', config: server === 'on' ? { prune_platform_copies: true } : {} }];
  win.fetch = makeFetchMock((url, options) => (options.method === 'PUT'
    ? (response || { ok: true, status: 200 })
    : { ok: server !== 'fail', status: server === 'fail' ? 500 : 200, json: { rules } }));
  const toasts = [];
  win.showToast = (m) => toasts.push(m);
  const errors = [];
  win.console.error = (...a) => errors.push(a.join(' '));
  const dialogs = [];
  if (dialog === 'record') {
    win.showConfirmDialog = (message, key, onConfirm, opts) => dialogs.push({ message, key, onConfirm, opts });
  }
  win.eval(RULE_TOGGLE_SRC);

  const form = win.document.querySelector('form');
  const sw = win.document.querySelector('[data-prune-platform-copies]');
  const panel = win.document.getElementById('rule-cfg-image_duplicate');
  const submit = () => win.handleRuleConfigSubmit({ preventDefault() {}, target: form });
  const form2 = () => {
    const f = win.document.createElement('form');
    f.dataset.ruleId = 'other';
    win.document.body.appendChild(f);
    return f;
  };
  const puts = () => win.fetch.calls.filter((c) => c.options.method === 'PUT');
  const sentConfigs = () => puts().map((c) => JSON.parse(c.options.body).config);
  return { win, sw, panel, form, form2, submit, sentConfigs, puts, toasts, errors, dialogs };
}

// holdGet makes the stale-page GET wait until release(); PUTs answer at once.
function holdGet(win, rules) {
  const calls = [];
  let release;
  win.fetch = (url, options) => {
    calls.push({ url, options });
    if (options.method === 'PUT') return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}), text: () => Promise.resolve('') });
    return new Promise((r) => { release = () => r({ ok: true, status: 200, json: () => Promise.resolve({ rules }) }); });
  };
  win.fetch.calls = calls;
  return () => release();
}

const REFUSED_TOAST = 'Not saved. The server cleanup cannot run at this similarity threshold. Turn the switch off to save.';
const isOn = (sw) => sw.getAttribute('aria-checked') === 'true';

describe('prune switch: clicking flips local state only', () => {
  it('turns on and off without sending anything', () => {
    const { win, sw } = setup({ stored: false });
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), true, 'first click turns the switch on');
    assert.equal(sw.getAttribute('class'), 'BTN-ON');
    assert.equal(sw.querySelector('span').getAttribute('class'), 'KNOB-ON');
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), false, 'second click turns it back off');
    assert.equal(win.fetch.calls.length, 0, 'a click must not save');
  });
});

describe('prune switch: off to on asks first', () => {
  it('sends nothing until the dialog is accepted, then sends the option, tolerance and severity', async () => {
    const { win, sw, panel, submit, sentConfigs, dialogs } = setup({ stored: false });
    win.togglePrunePlatformCopies(sw);
    submit();
    await flush();

    assert.equal(dialogs.length, 1, 'Save must open the confirm dialog');
    assert.equal(win.fetch.calls.length, 0, 'nothing may be sent before the dialog is accepted');
    assert.equal(dialogs[0].message, 'BODY');
    assert.equal(dialogs[0].key, null, 'a null key hides "Don\'t ask again"');
    assert.deepEqual({ ...dialogs[0].opts }, { title: 'TITLE', acceptText: 'ACCEPT' });

    dialogs[0].onConfirm();
    await flush();
    assert.equal(win.fetch.calls.length, 1);
    assert.match(win.fetch.calls[0].url, /\/api\/v1\/rules\/image_duplicate$/);
    assert.deepEqual(sentConfigs()[0], { tolerance: 0.95, severity: 'warning', prune_platform_copies: true });
    assert.equal(panel.classList.contains('hidden'), true, 'the panel closes after a successful save');
    assert.equal(sw.dataset.initial, 'true', 'the stored baseline follows the save');
  });

  it('cancel sends nothing and leaves the panel open with the switch still on', async () => {
    const { win, sw, panel, submit, dialogs, toasts } = setup({ stored: false });
    win.togglePrunePlatformCopies(sw);
    submit();
    await flush();
    assert.equal(dialogs.length, 1);
    assert.equal(win.fetch.calls.length, 0, 'a cancelled confirm must send no request');
    assert.equal(panel.classList.contains('hidden'), false, 'the panel stays open');
    assert.equal(isOn(sw), true, 'the switch stays as the user left it');
    assert.equal(sw.dataset.initial, 'false');
    assert.equal(toasts.length, 0, 'cancel is not an error');
  });

  it('a failed save keeps the panel open, the switch on, and shows the failure toast', async () => {
    const { win, sw, panel, submit, dialogs, toasts, errors } = setup({ stored: false, response: { ok: false, status: 500 } });
    win.togglePrunePlatformCopies(sw);
    submit();
    dialogs[0].onConfirm();
    await flush();
    assert.equal(win.fetch.calls.length, 1);
    assert.deepEqual(toasts, ['Failed to update rule.']);
    assert.match(errors.join('\n'), /saving the rule config failed: Error: Failed to update rule/);
    assert.equal(panel.classList.contains('hidden'), false);
    assert.equal(isOn(sw), true);
    assert.equal(sw.dataset.initial, 'false', 'a failed save must not move the stored baseline');
  });
});

describe('prune switch: stored on, switch on re-checks the server first', () => {
  const ON = [{ id: 'image_duplicate', config: { prune_platform_copies: true } }];

  it('server still on: sends true without asking, after one GET', async () => {
    const { win, submit, sentConfigs, dialogs } = setup({ stored: true });
    submit();
    await flush();
    assert.equal(dialogs.length, 0, 'already on: no dialog');
    assert.equal(win.fetch.calls.length, 2, 'one GET to check the stored value, then the PUT');
    assert.match(win.fetch.calls[0].url, /\/api\/v1\/rules$/);
    assert.equal(win.fetch.calls[0].options.cache, 'no-store');
    assert.deepEqual(sentConfigs(), [{ tolerance: 0.95, severity: 'warning', prune_platform_copies: true }]);
  });

  it('turned off elsewhere: asks; cancel sends no PUT, accept sends it', async () => {
    const { sw, submit, sentConfigs, puts, dialogs } = setup({ stored: true, server: 'off' });
    submit();
    await flush();
    assert.equal(dialogs.length, 1, 'the server says off, so this save turns deletion on: ask');
    assert.equal(puts().length, 0);
    assert.equal(sw.dataset.initial, 'false', 'the stale baseline is corrected');
    dialogs[0].onConfirm();
    await flush();
    assert.deepEqual(sentConfigs(), [{ tolerance: 0.95, severity: 'warning', prune_platform_copies: true }]);
  });

  it('stored value unreadable: sends nothing and says so, even with no toast API', async () => {
    const { win, submit, puts, dialogs, errors } = setup({ stored: true, server: 'fail' });
    delete win.showToast;
    submit();
    await flush();
    assert.equal(puts().length, 0, 'fail closed');
    assert.equal(dialogs.length, 0);
    assert.match(errors.join('\n'), /could not confirm the stored option/);
    assert.match(errors.join('\n'), /showToast unavailable/);
  });

  it('the guard is held during the GET: a click and a Save then change and send nothing', async () => {
    const { win, sw, submit, puts } = setup({ stored: true });
    const release = holdGet(win, ON);
    submit();
    win.togglePrunePlatformCopies(sw);
    submit();
    await flush();
    assert.equal(isOn(sw), true, 'the switch is locked while the GET is out');
    assert.equal(puts().length, 0, 'a Save during the GET must not send');
    release();
    await flush();
    assert.equal(puts().length, 1);
  });

  it('the guard is released when the GET fails', async () => {
    const { win, sw, submit, puts } = setup({ stored: true, server: 'fail' });
    submit();
    await flush();
    win.togglePrunePlatformCopies(sw); // off: saves without a GET
    submit();
    await flush();
    assert.equal(puts().length, 1, 'a failed GET must not leave the form dead');
  });

  it('a rule missing from the list is not treated as stored on', async () => {
    const { submit, puts, dialogs, errors } = setup({ stored: true, rulesList: [{ id: 'someone_else', config: { prune_platform_copies: true } }] });
    submit();
    await flush();
    assert.equal(puts().length, 0);
    assert.equal(dialogs.length, 0);
    assert.match(errors.join('\n'), /rule missing or malformed/);
  });

  it('a field edited during the GET is what the PUT carries; the next Save checks again', async () => {
    const { win, submit, sentConfigs, form } = setup({ stored: true });
    const release = holdGet(win, ON);
    submit();
    form.elements.severity.value = 'error';
    release(); await flush();
    assert.deepEqual(sentConfigs().map((c) => c.severity), ['error']);
    assert.equal('pruneRechecked' in form.dataset, false, 'the one-shot marker is consumed');
    submit(); await flush();
    assert.equal(win.fetch.calls.filter((c) => c.options.method !== 'PUT').length, 2, 'the second Save reads the server again');
  });

  it('the marker is cleared even when the re-entry returns early', async () => {
    const { win, submit, puts, form } = setup({ stored: true });
    form.dataset.pruneRechecked = '1';
    form.dataset.inflight = '1';
    submit();
    assert.equal('pruneRechecked' in form.dataset, false);
    delete form.dataset.inflight;
    submit(); await flush();
    assert.equal(win.fetch.calls[0].options.method !== 'PUT', true, 'a later Save still starts with the GET');
    assert.equal(puts().length, 1);
  });

  it('a malformed stored answer fails closed: no PUT, no dialog, guard released, failure toast', async () => {
    for (const config of [[], 'on', { prune_platform_copies: 'true' }, { prune_platform_copies: 1 }]) {
      const { sw, win, submit, puts, dialogs, toasts } = setup({ stored: true, rulesList: [{ id: 'image_duplicate', config }] });
      submit(); await flush();
      assert.equal(puts().length + dialogs.length, 0, JSON.stringify(config));
      assert.deepEqual(toasts, ['Failed to update rule.']);
      win.togglePrunePlatformCopies(sw); // unlocked again: the guard was released
      assert.equal(isOn(sw), false);
    }
  });

  it('the rule is picked by id, not by position', async () => {
    const { submit, puts, dialogs } = setup({ stored: true, rulesList: [
      { id: 'first_rule', config: { prune_platform_copies: true } },
      { id: 'image_duplicate', config: {} },
    ] });
    submit();
    await flush();
    assert.equal(dialogs.length, 1, 'the target rule is off on the server, so Save must ask');
    assert.equal(puts().length, 0);
  });
});

describe('prune switch: more saves that need no dialog', () => {
  it('a Save dropped while another is in flight says so, and another form still saves', async () => {
    const { win, sw, submit, toasts, errors, puts } = setup({ stored: true });
    win.togglePrunePlatformCopies(sw); // off: saves without a dialog
    const held = win.fetch;
    win.fetch = (url, options) => { held.calls.push({ url, options }); return new Promise(() => {}); };
    win.fetch.calls = held.calls;
    submit();
    submit();
    assert.deepEqual(toasts, ['A save is already in progress. Try again in a moment.']);
    assert.match(errors.join('\n'), /a save is already in progress/);
    delete win.showToast;
    submit();
    assert.equal(errors.length, 2, 'without showToast the drop is still logged');
    const other = win.document.createElement('form');
    other.dataset.ruleId = 'other';
    win.handleRuleConfigSubmit({ preventDefault() {}, target: other });
    assert.equal(puts().length, 2, 'the guard is per form');
  });
});

describe('prune switch: a save or an open confirmation holds the form', () => {
  function holdPuts(win) { // later requests wait; returns release()
    const held = win.fetch;
    let rel = () => {};
    win.fetch = (url, options) => { held.calls.push({ url, options }); return new Promise((r) => { rel = r; }); };
    win.fetch.calls = held.calls;
    return (v) => rel(v);
  }
  it('a click during an in-flight save changes nothing; after it settles a click works', async () => {
    const { win, sw, submit } = setup({ stored: true });
    win.togglePrunePlatformCopies(sw);
    const release = holdPuts(win);
    submit();
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), false, 'a click during the save must not flip the switch');
    release({ ok: true, status: 200 }); await flush();
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), true);
  });
  it('dialog open then an off-save: accepting late sends no ON, in flight or completed', async () => {
    for (const hold of [true, false]) {
      const { win, sw, submit, dialogs, puts, sentConfigs, errors } = setup({ stored: false });
      win.togglePrunePlatformCopies(sw);
      submit();
      win.togglePrunePlatformCopies(sw); // off
      if (hold) holdPuts(win);
      submit();
      await flush();
      dialogs[0].onConfirm(); await flush();
      assert.equal(puts().length, 1, 'only the off-save is sent, held: ' + hold);
      assert.equal('prune_platform_copies' in sentConfigs()[0], false);
      assert.match(errors.join('\n'), /form changed while/);
    }
  });
  it('cancel leaves the form usable, and accept sends one PUT carrying true', async () => {
    const { win, sw, submit, dialogs, sentConfigs } = setup({ stored: false });
    win.togglePrunePlatformCopies(sw);
    submit(); submit();
    assert.equal(dialogs.length, 2, 'no stuck reservation');
    holdPuts(win);
    dialogs[1].onConfirm(); dialogs[0].onConfirm(); await flush(); // a second accept while the first PUT is in flight
    assert.deepEqual(sentConfigs().map((c) => c.prune_platform_copies), [true]);
  });
});

describe('prune switch: saves that need no dialog', () => {
  it('the guard is released after a FAILED PUT: a later Save works', async () => {
    const { submit, puts } = setup({ stored: false, response: { ok: false, status: 500 } });
    submit();
    await flush();
    submit();
    await flush();
    assert.equal(puts().length, 2, 'after a failed save the form must accept the next Save');
  });

  it('a second Save while one is in flight sends nothing; a later Save works', async () => {
    const { win, sw, submit, puts } = setup({ stored: true });
    win.togglePrunePlatformCopies(sw); // off: no dialog
    let release;
    const held = win.fetch;
    win.fetch = (url, options) => { held.calls.push({ url, options }); return new Promise((r) => { release = r; }); };
    win.fetch.calls = held.calls;
    submit();
    submit();
    await flush();
    assert.equal(puts().length, 1, 'the second submit must not send while the first is unresolved');
    release({ ok: true, status: 200 });
    await flush();
    submit();
    assert.equal(puts().length, 2, 'once the first save settles, Save works again');
  });

  it('on to off sends without asking and omits the key', async () => {
    const { win, sw, submit, sentConfigs, dialogs } = setup({ stored: true });
    win.togglePrunePlatformCopies(sw);
    submit();
    await flush();
    assert.equal(dialogs.length, 0, 'turning off: no dialog');
    assert.equal(sentConfigs().length, 1);
    assert.equal('prune_platform_copies' in sentConfigs()[0], false, 'off must OMIT the key, not send false or true');
    assert.deepEqual(sentConfigs()[0], { tolerance: 0.95, severity: 'warning' });
    assert.equal(sw.dataset.initial, 'false');
  });

  it('off to off sends without asking, and an unset tolerance stays unset', async () => {
    const { submit, sentConfigs, dialogs } = setup({ stored: false, tolerance: '' });
    submit();
    await flush();
    assert.equal(dialogs.length, 0);
    assert.deepEqual(sentConfigs(), [{ severity: 'warning' }]);
  });
});

describe('prune switch: fails closed', () => {
  it('sends nothing when showConfirmDialog is missing, and says so loudly', async () => {
    const { win, sw, panel, submit, toasts, errors } = setup({ stored: false, dialog: 'missing' });
    assert.equal(typeof win.showConfirmDialog, 'undefined', 'precondition: the dialog API is absent');
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), true, 'precondition: the switch is on, so Save would turn the option on');
    submit();
    await flush();
    assert.equal(win.fetch.calls.length, 0, 'no dialog means no consent: nothing may be sent');
    assert.deepEqual(toasts, ['Failed to update rule.']);
    assert.equal(errors.length, 1, 'the missing capability must be logged with console.error');
    assert.match(errors[0], /showConfirmDialog unavailable/);
    assert.equal(panel.classList.contains('hidden'), false);
  });

  it('refused tolerance: a click cannot turn it on, and a forced-on switch still sends nothing', async () => {
    const { sw, submit, dialogs, toasts, errors, win } = setup({ stored: false, blocked: true, tolerance: '0.8' });
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), false, 'a blocked switch must stay off');
    // Force it past the click guard (stale page, devtools): submit must hold alone.
    sw.setAttribute('aria-checked', 'true');
    submit();
    await flush();
    assert.equal(win.fetch.calls.length, 0, 'a refused tolerance must send nothing');
    assert.equal(dialogs.length, 0, 'and must not ask for a consent that does nothing');
    assert.deepEqual(toasts, [REFUSED_TOAST]);
    assert.equal(errors.length, 1);
  });

  it('stored on, server answering on, no dialog API: saves, since this save turns nothing on', async () => {
    const { submit, puts } = setup({ stored: true, dialog: 'missing' });
    submit();
    await flush();
    assert.equal(puts().length, 1);
  });

  it('stored on, server answering off: a missing dialog or a refused threshold sends nothing', async () => {
    const a = setup({ stored: true, server: 'off', dialog: 'missing' });
    a.submit();
    const b = setup({ stored: true, server: 'off', blocked: true, tolerance: '0.8' });
    b.submit();
    await flush();
    assert.equal(a.puts().length, 0, 'no dialog, no consent');
    assert.match(a.errors.join('\n'), /showConfirmDialog unavailable/);
    assert.equal(b.puts().length, 0, 'a refused threshold blocks the save');
    assert.deepEqual(b.toasts, [REFUSED_TOAST]);
  });

  it('refused tolerance: turning OFF always works, and can be undone before Save', async () => {
    const { win, sw, submit, sentConfigs, dialogs } = setup({ stored: true, blocked: true, tolerance: '0.8' });
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), false, 'stored on + refused: the switch can be turned off');
    assert.equal(sw.hasAttribute('aria-disabled'), false, 'not locked while the stored value is still on');
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), true, 'an unsaved turn-off can be undone');
    win.togglePrunePlatformCopies(sw);
    submit();
    await flush();
    assert.equal(dialogs.length, 0);
    assert.deepEqual(sentConfigs(), [{ tolerance: 0.8, severity: 'warning' }]);
    assert.equal(sw.getAttribute('aria-disabled'), 'true', 'once saved off, the blocked switch locks');
    assert.equal(sw.classList.contains('cursor-not-allowed'), true, 'and shows the not-allowed cursor');
    win.togglePrunePlatformCopies(sw);
    assert.equal(isOn(sw), false, 'and can no longer be turned on');
  });
});

