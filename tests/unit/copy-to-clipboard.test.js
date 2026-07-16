// Regression tests for window.swCopyToClipboard (web/templates/layout.templ,
// issue #2526).
//
// navigator.clipboard is undefined outside a secure context (HTTPS or
// localhost) -- the common case for Stillwater's self-hosted plain-HTTP LAN
// deployments. swCopyToClipboard feature-detects the Clipboard API and falls
// back to document.execCommand('copy'), rejecting only when both paths fail
// so callers can surface a loud error toast instead of copy silently
// no-oping.
//
// swCopyToClipboard is embedded inline in layout.templ (not a standalone
// static JS file). We inline it here as a literal string, matching the
// established convention for inline templ scripts (see
// column-toggle-localstorage-guard.test.js). It must be kept in sync with
// the production script in web/templates/layout.templ.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { JSDOM } from 'jsdom';

const COPY_TO_CLIPBOARD_SCRIPT = `
window.swCopyToClipboard = function(text) {
	function execCommandFallback() {
		return new Promise(function(resolve, reject) {
			var ta = document.createElement('textarea');
			try {
				ta.value = text;
				ta.style.position = 'fixed';
				ta.style.opacity = '0';
				document.body.appendChild(ta);
				ta.select();
				var ok = document.execCommand('copy');
				if (ok) resolve(); else reject(new Error('execCommand copy failed'));
			} catch (e) {
				reject(e);
			} finally {
				if (ta.parentNode) ta.parentNode.removeChild(ta);
			}
		});
	}
	if (navigator.clipboard && navigator.clipboard.writeText) {
		return navigator.clipboard.writeText(text).catch(execCommandFallback);
	}
	return execCommandFallback();
};
`;

function setupDom() {
  const dom = new JSDOM('<!doctype html><html><body></body></html>', {
    runScripts: 'dangerously',
    url: 'http://localhost:1973/',
  });
  dom.window.eval(COPY_TO_CLIPBOARD_SCRIPT);
  return dom;
}

describe('swCopyToClipboard: Clipboard API available (secure context)', () => {
  it('resolves via navigator.clipboard.writeText without touching the DOM fallback', async () => {
    const dom = setupDom();
    const win = dom.window;

    const calls = [];
    win.navigator.clipboard = {
      writeText: (text) => {
        calls.push(text);
        return Promise.resolve();
      },
    };

    await assert.doesNotReject(win.swCopyToClipboard('hello'));
    assert.deepEqual(calls, ['hello']);
    // No leftover textarea from the fallback path.
    assert.equal(win.document.querySelectorAll('textarea').length, 0);
  });
});

describe('swCopyToClipboard: Clipboard API absent (plain-HTTP LAN deployment, #2526)', () => {
  it('falls back to execCommand and resolves when the fallback succeeds', async () => {
    const dom = setupDom();
    const win = dom.window;

    // navigator.clipboard is undefined outside a secure context.
    assert.equal(win.navigator.clipboard, undefined);
    win.document.execCommand = () => true;

    await assert.doesNotReject(win.swCopyToClipboard('fallback text'));
    // The temporary textarea must be cleaned up after a successful copy.
    assert.equal(win.document.querySelectorAll('textarea').length, 0);
  });
});

describe('swCopyToClipboard: Clipboard API rejects, execCommand fallback succeeds', () => {
  it('resolves via the fallback after the async API rejects', async () => {
    const dom = setupDom();
    const win = dom.window;

    win.navigator.clipboard = {
      writeText: () => Promise.reject(new Error('denied')),
    };
    win.document.execCommand = () => true;

    await assert.doesNotReject(win.swCopyToClipboard('retry text'));
  });
});

describe('swCopyToClipboard: both paths fail (double-failure, the silent-copy bug class)', () => {
  it('rejects when execCommand returns false (no throw) with the Clipboard API absent', async () => {
    const dom = setupDom();
    const win = dom.window;

    assert.equal(win.navigator.clipboard, undefined);
    // execCommand can fail WITHOUT throwing -- a bare boolean false. The bug
    // this guards against (#2526 fix-round F1) is treating that as success.
    win.document.execCommand = () => false;

    await assert.rejects(win.swCopyToClipboard('unreachable text'));
    // Even on failure, the temporary textarea must not leak into the DOM
    // (fix-round F4).
    assert.equal(win.document.querySelectorAll('textarea').length, 0);
  });

  it('rejects when execCommand throws, with the Clipboard API absent', async () => {
    const dom = setupDom();
    const win = dom.window;

    assert.equal(win.navigator.clipboard, undefined);
    win.document.execCommand = () => { throw new Error('boom'); };

    await assert.rejects(win.swCopyToClipboard('unreachable text'));
    assert.equal(win.document.querySelectorAll('textarea').length, 0);
  });

  it('rejects when both the async API and execCommand fail', async () => {
    const dom = setupDom();
    const win = dom.window;

    win.navigator.clipboard = {
      writeText: () => Promise.reject(new Error('denied')),
    };
    win.document.execCommand = () => false;

    await assert.rejects(win.swCopyToClipboard('unreachable text'));
    assert.equal(win.document.querySelectorAll('textarea').length, 0);
  });
});
