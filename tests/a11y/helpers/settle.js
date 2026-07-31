// settle.js - shared a11y test helpers for reading SETTLED rendered state.

// disableTransitions injects a stylesheet (on every navigation) that turns off
// CSS transitions and animations for the whole page, so a synchronous
// getComputedStyle read (axe's color-contrast rule) never samples a
// mid-transition blended color and reports a false WCAG failure. Used by the
// a11y specs' beforeEach; the settled colors it exposes are the real ones.
// The stylesheet is attached to <head> once the parser has built it. The
// previous form appended to document.documentElement from inside the init
// script, before <head> existed: the node was discarded when the parser built
// the real tree, so NO stylesheet ever landed and this helper was a silent
// no-op (measured on both engines -- zero matching <style> elements in the
// document, transition-duration still 0.15s). Every spec calling it believed
// it was reading settled colors and was not.
export async function disableTransitions(page) {
  await page.addInitScript(() => {
    const CSS_TEXT =
      '*, *::before, *::after { transition: none !important; animation: none !important; }';
    const attach = () => {
      const parent = document.head || document.documentElement;
      if (!parent) return false;
      const style = document.createElement('style');
      style.setAttribute('data-sw-disable-transitions', '');
      style.textContent = CSS_TEXT;
      parent.appendChild(style);
      return true;
    };
    // <head> may or may not exist yet depending on how early the init script
    // runs, so try now and fall back to the parser milestone. Loudly, not
    // silently: a helper that quietly does nothing is what this replaces.
    if (!attach()) {
      document.addEventListener('DOMContentLoaded', () => {
        if (!attach()) {
          console.error('disableTransitions: could not attach the stylesheet; '
            + 'computed-style reads in this test may sample mid-transition values');
        }
      }, { once: true });
    }
  });
}
