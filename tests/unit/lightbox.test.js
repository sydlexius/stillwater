// Unit tests for web/static/js/artist-detail/lightbox.js
// Covers: open/close state, focus management, and the Tab/Shift+Tab focus-trap
// (WCAG 2.1 SC 2.4.3 keyboard focus containment).
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom } from './helpers/dom-harness.js';

// Lightbox overlay + two focusable elements inside it.
// The module queries for focusable elements using its own selector list;
// buttons are natively in that list so no tabindex is required.
const LIGHTBOX_HTML = `<!doctype html><html><body>
<button id="outside-btn">Trigger</button>
<div id="sw-lightbox" class="hidden">
  <button data-lightbox-close id="close-btn">Close</button>
  <button id="extra-btn">Extra action</button>
  <img id="sw-lightbox-img" src="" alt="">
</div>
</body></html>`;

function openedDom() {
  const dom = createDom({ html: LIGHTBOX_HTML, modules: ['lightbox'] });
  dom.window.swLightbox.open('http://example.com/img.jpg', 'test alt');
  return dom;
}

// ---------------------------------------------------------------------------
// open / close lifecycle
// ---------------------------------------------------------------------------
describe('lightbox: open and close lifecycle', () => {
  it('open removes hidden class and adds flex class', () => {
    const dom = openedDom();
    const lb = dom.window.document.getElementById('sw-lightbox');
    assert.ok(!lb.classList.contains('hidden'), 'lightbox must not have hidden class when open');
    assert.ok(lb.classList.contains('flex'), 'lightbox must have flex class when open');
  });

  it('open sets the img src and alt', () => {
    const dom = openedDom();
    const img = dom.window.document.getElementById('sw-lightbox-img');
    assert.equal(img.src, 'http://example.com/img.jpg');
    assert.equal(img.alt, 'test alt');
  });

  it('open moves focus to the close button', () => {
    const dom = openedDom();
    const closeBtn = dom.window.document.getElementById('close-btn');
    assert.equal(dom.window.document.activeElement, closeBtn,
      'focus must move to [data-lightbox-close] when lightbox opens');
  });

  it('close adds hidden class and removes flex class', () => {
    const dom = openedDom();
    dom.window.swLightbox.close();
    const lb = dom.window.document.getElementById('sw-lightbox');
    assert.ok(lb.classList.contains('hidden'), 'lightbox must have hidden class when closed');
    assert.ok(!lb.classList.contains('flex'), 'lightbox must not have flex class when closed');
  });

  it('close clears the img src attribute', () => {
    const dom = openedDom();
    dom.window.swLightbox.close();
    const img = dom.window.document.getElementById('sw-lightbox-img');
    // Check the raw attribute rather than the resolved .src property;
    // jsdom resolves an empty string to the base URL in the .src getter.
    assert.equal(img.getAttribute('src'), '', 'img src attribute must be cleared on close');
  });

  it('close restores focus to the element that was active before open', () => {
    const dom = createDom({ html: LIGHTBOX_HTML, modules: ['lightbox'] });
    const outsideBtn = dom.window.document.getElementById('outside-btn');
    outsideBtn.focus();

    dom.window.swLightbox.open('img.jpg', '');
    dom.window.swLightbox.close();

    assert.equal(dom.window.document.activeElement, outsideBtn,
      'focus must return to the opener element after close');
  });
});

// ---------------------------------------------------------------------------
// Keyboard: Escape
// ---------------------------------------------------------------------------
describe('lightbox: Escape key closes the lightbox', () => {
  it('Escape closes the lightbox and prevents default', () => {
    const dom = openedDom();
    const lb = dom.window.document.getElementById('sw-lightbox');

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Escape', bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.ok(lb.classList.contains('hidden'), 'Escape must close the lightbox');
    assert.ok(evt.defaultPrevented, 'Escape must call preventDefault');
  });
});

// ---------------------------------------------------------------------------
// Keyboard: Tab focus-trap
// ---------------------------------------------------------------------------
describe('lightbox: Tab focus-trap', () => {
  it('Tab on last focusable element wraps focus to first', () => {
    const dom = openedDom();
    const lb = dom.window.document.getElementById('sw-lightbox');

    // Find the same focusables the module finds.
    const focusables = lb.querySelectorAll(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]),' +
      ' textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    const first = focusables[0];
    const last  = focusables[focusables.length - 1];

    last.focus();
    assert.equal(dom.window.document.activeElement, last, 'precondition: last element is focused');

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Tab', shiftKey: false, bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.equal(dom.window.document.activeElement, first,
      'Tab on last element must wrap focus to first');
    assert.ok(evt.defaultPrevented, 'Tab wrap must call preventDefault');
  });

  it('Shift+Tab on first focusable element wraps focus to last', () => {
    const dom = openedDom();
    const lb = dom.window.document.getElementById('sw-lightbox');

    const focusables = lb.querySelectorAll(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]),' +
      ' textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    const first = focusables[0];
    const last  = focusables[focusables.length - 1];

    first.focus();
    assert.equal(dom.window.document.activeElement, first, 'precondition: first element is focused');

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Tab', shiftKey: true, bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.equal(dom.window.document.activeElement, last,
      'Shift+Tab on first element must wrap focus to last');
    assert.ok(evt.defaultPrevented, 'Shift+Tab wrap must call preventDefault');
  });

  it('Tab on a middle element does not trap (natural order continues)', () => {
    const dom = openedDom();
    const lb = dom.window.document.getElementById('sw-lightbox');

    const focusables = lb.querySelectorAll(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]),' +
      ' textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    // Only two buttons; skip this case when there are not enough elements.
    if (focusables.length < 2) return;

    // Focus the first element (not the last), Tab should NOT preventDefault.
    const first = focusables[0];
    first.focus();

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Tab', shiftKey: false, bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    // The module only prevents default when wrapping; Tab from first (not last) passes through.
    assert.ok(!evt.defaultPrevented,
      'Tab from first element (not last) must not call preventDefault');
  });

  it('Tab key outside open lightbox is not intercepted', () => {
    const dom = createDom({ html: LIGHTBOX_HTML, modules: ['lightbox'] });
    // Lightbox is NOT opened — no keydown listener should be active.
    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Tab', bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);
    assert.ok(!evt.defaultPrevented, 'Tab must not be intercepted when lightbox is closed');
  });
});
