// settle.js - shared a11y test helpers for reading SETTLED rendered state.

// disableTransitions injects a stylesheet (on every navigation) that turns off
// CSS transitions and animations for the whole page, so a synchronous
// getComputedStyle read (axe's color-contrast rule) never samples a
// mid-transition blended color and reports a false WCAG failure. Used by the
// a11y specs' beforeEach; the settled colors it exposes are the real ones.
export async function disableTransitions(page) {
  await page.addInitScript(() => {
    const style = document.createElement('style');
    style.textContent =
      '*, *::before, *::after { transition: none !important; animation: none !important; }';
    document.documentElement.appendChild(style);
  });
}
