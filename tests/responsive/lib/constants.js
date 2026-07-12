// constants.js - shared tokens for the responsive UAT harness (issue #2386).
//
// VIEWPORTS: the three breakpoints every mobile/UI issue in this milestone
// family measures against (small phone, large phone, tablet).
//
// TAP_TARGET_MIN_PX: minimum interactive hit-target size in CSS px. This is a
// PLACEHOLDER pending issue #2377, which is pinning the real value as a design
// token. #2377 owns the RULE (what the minimum should be); this harness only
// MEASURES against whatever that constant says. When #2377 lands, swap this
// import for the real token -- do not redefine a competing value elsewhere in
// this harness.
export const TAP_TARGET_MIN_PX = 44;

// width/height are CSS px (Playwright viewport size, not device pixels).
export const VIEWPORTS = [
  { name: 'mobile-360', width: 360, height: 740 },
  { name: 'mobile-390', width: 390, height: 844 },
  { name: 'tablet-768', width: 768, height: 1024 },
];

export const THEMES = ['dark', 'light'];

// Dark is the app's DEFAULT theme (see web/static/js/preferences.js). Any
// caller that skips setTheme() and merely navigates gets whatever the OS
// reports via matchMedia, which is NOT deterministic evidence -- always call
// setTheme() explicitly for both themes.
export const DEFAULT_THEME = 'dark';
