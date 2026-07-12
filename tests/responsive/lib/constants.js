// constants.js - shared tokens for the responsive UAT harness (issue #2386).
//
// VIEWPORTS: the six breakpoints every mobile/UI issue in this milestone
// family measures against (smallest realistic phone through tablet).
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
  { name: 'mobile-320', width: 320, height: 568 },
  { name: 'mobile-360', width: 360, height: 740 },
  { name: 'mobile-390', width: 390, height: 844 },
  { name: 'mobile-414', width: 414, height: 896 },
  { name: 'mobile-430', width: 430, height: 932 },
  { name: 'tablet-768', width: 768, height: 1024 },
];

export const THEMES = ['dark', 'light'];

// Every element a user can tap. The pre-fix set ('a, button, [role=button],
// input, select') silently missed textarea, summary, and the ARIA widget roles
// this app actually ships (switch/tab/menuitem), plus anything made focusable
// with a positive tabindex -- so the tap-target census undercounted the very
// controls the mobile milestone exists to enlarge. `a` is narrowed to
// `a[href]`: an anchor without an href is not an interactive target.
export const INTERACTIVE_SELECTOR = [
  'a[href]',
  'button',
  'input',
  'select',
  'textarea',
  'summary',
  '[role="button"]',
  '[role="link"]',
  '[role="switch"]',
  '[role="tab"]',
  '[role="menuitem"]',
  '[role="checkbox"]',
  '[role="radio"]',
  '[tabindex]:not([tabindex="-1"])',
].join(', ');

// The positive marker that a rendered page is an AUTHENTICATED app shell and
// not the login page. Set on <body> by web/templates/layout.templ (the shared
// layout behind both the /next and legacy route sets); web/templates/login.templ
// deliberately does not carry it.
//
// This exists because a logged-out request to an app route returns HTTP 200 at
// the SAME URL with the login page in the body -- so status and URL alone
// cannot tell "measured the settings screen" from "measured a login form".
// See assertAuthenticated (lib/page-guards.js).
export const AUTHED_BODY_CLASS = 'sw-next-shell';

// The login page's <title>. Belt-and-suspenders alongside AUTHED_BODY_CLASS:
// if the shell class is ever added to the login layout by accident, the title
// still catches it.
export const LOGIN_TITLE_PREFIX = 'Login';

// Dark is the app's DEFAULT theme (see web/static/js/preferences.js). Any
// caller that skips setTheme() and merely navigates gets whatever the OS
// reports via matchMedia, which is NOT deterministic evidence -- always call
// setTheme() explicitly for both themes.
export const DEFAULT_THEME = 'dark';
