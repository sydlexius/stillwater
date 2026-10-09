// muted-text-contrast.spec.js - the muted secondary-text color must meet WCAG AA
// (4.5:1) as PAINTED, in both themes, on the settings, onboarding and register
// surfaces (#3474, slice 3a).
//
// WHY PAINTED, NOT axe: the old muted pair (gray-400 light / gray-500 dark)
// sits on translucent glass cards over an image. axe reports text over a
// translucent surface as "incomplete" (never pass/fail) and never evaluates
// :hover, so an axe scan of these pages is clean while the text is unreadable.
// renderedContrast (helpers/axe.js) scores the minimum glyph pixel from real
// screenshots instead.
//
// FIXTURE: helpers/seed-muted-text.js boots two THROWAWAY servers (the shared
// harness server has onboarding complete and multi-user off, so /setup/wizard
// redirects and /register is a 404). Nothing here touches the shared server, so
// nothing this spec seeds is visible to, or outlives, any other spec.
//
// SITES MEASURED (file : what) and SITES THAT CANNOT BE REACHED under the
// harness. Every muted-text site in the five templates is listed.
//   settings.templ
//     1108 provider rate-limit label ........ REACHED  (provider cards)
//     1304,1318 MusicBrainz server hints .... REACHED  (config panel opened)
//     1384 custom-mirror help ............... REACHED  (custom radio chosen)
//     1390,1395,1400,1410 OAuth block ....... REACHED  (config panel opened)
//     1445 verbosity description ............ REACHED  (Wikipedia panel opened)
//     1735 priority-row instructions ........ REACHED
//     1798 "no providers" for a field ....... UNREACHABLE: needs a priority field
//          with zero available providers; keyless providers (MusicBrainz,
//          Wikipedia, ...) are always available, so no field is ever empty.
//     2386 NFO "Disabled" (platform card) ... REACHED
//     2415 rule catalogue link (+ hover) .... REACHED
//     2921 "up to date" .................... UNREACHABLE: needs a completed update
//          check, which asks GitHub for the latest release; the harness is offline
//          by design and the updater has no fixture seam.
//     2924 "Not yet checked" ................ REACHED
//     2932 "Last checked" row ............... UNREACHABLE: same update check; the
//          row is hidden until a check has completed.
//   settings_sections.templ
//     436 "no libraries" .................... REACHED  (fixture deletes its libraries)
//     906,923 rule sub-headings ............. REACHED
//   settings_sections_next.templ
//     486 path-mapping arrow glyph .......... REACHED  (fixture platform connection)
//   onboarding.templ
//     209 no-libraries, server copy ......... REACHED
//     1059 no-libraries, JS innerHTML copy .. REACHED  (Remove clicked in the wizard)
//     283 language-skip consequence ......... REACHED
//     1722 profile card NFO "Disabled" ...... REACHED
//     1950 connection info button (+ hover) . REACHED
//   register.templ
//     323 "(optional)" ...................... REACHED  (valid invite code)
import { test, expect } from 'playwright/test';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { disableTransitions } from './helpers/settle.js';
import { applyTheme, renderedContrast } from './helpers/axe.js';
import { startSettingsFixture, startOnboardingFixture, addConnection, removeConnection } from './helpers/seed-muted-text.js';

const AA = 4.5;
const rAF2 = () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));

let settingsFx;
let onboardingFx;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  settingsFx = await startSettingsFixture();
  onboardingFx = await startOnboardingFixture();
});

test.afterAll(() => {
  settingsFx?.server.stop();
  onboardingFx?.server.stop();
});

// open returns a fresh authenticated page on the fixture server, in a theme.
// useAppTheme=false is for pages without preferences.js (register): the theme
// there is only the `dark` class themeInitScript sets at load, so it is toggled.
async function open(browser, fx, urlPath, theme, useAppTheme = true) {
  const { server } = fx;
  const context = await browser.newContext({ colorScheme: 'dark', viewport: { width: 1280, height: 900 } });
  await context.addCookies([
    { name: 'session', value: server.sessionCookie, url: server.rootURL },
    { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
  ]);
  const page = await context.newPage();
  await disableTransitions(page);
  await page.goto(`${server.baseURL}${urlPath}`);
  await page.waitForLoadState('load');
  if (useAppTheme) {
    await applyTheme(expect, page, theme);
  } else {
    await page.evaluate((t) => document.documentElement.classList.toggle('dark', t === 'dark'), theme);
    await expect(page.locator('html')).toHaveClass(theme === 'dark' ? /\bdark\b/ : /^(?!.*\bdark\b).*$/);
  }
  return { context, page };
}

// measure scores EVERY element matched by loc (resting, and hovered when asked),
// after asserting the match count is at least `min`, and returns the name of
// each element under 4.5:1 (callers assert the whole list once, so one run names
// every failing site). Never skips: an absent site is a failing count.
//
// Two documented variants, both chosen per site:
//   ratio: 3   icon-only controls (an SVG stroked with currentColor). The text
//              threshold does not apply to a non-text graphic (WCAG 1.4.11 asks
//              3:1), and the minimum-glyph-pixel method reads a 1.5px stroke's
//              antialiased core below its true color (4.32 measured for gray-400
//              on the dark card, whose solid-fill contrast is about 5.9).
//   inactive   a label inside a deliberately disabled block (opacity-50, a
//              disabled input, a "Coming soon" badge). WCAG 1.4.3 exempts
//              inactive components, so the painted ratio is not the contract;
//              the contract is that the label resolves to the AA pair's color.
async function measure(page, theme, label, loc, { min, hover = false, ratio: threshold = AA, inactive = false }) {
  await expect.poll(() => loc.count(), { message: `${label}: want at least ${min} element(s)`, timeout: 15_000 })
    .toBeGreaterThanOrEqual(min);
  const n = await loc.count();
  const failures = [];
  for (let i = 0; i < n; i++) {
    const el = loc.nth(i);
    const text = ((await el.textContent()) || '').trim().slice(0, 40);
    const name = `${label}[${i}] ${JSON.stringify(text || (await el.getAttribute('aria-label')) || '(icon)')}`;
    await expect(el, `${name} must be visible to be measured`).toBeVisible();
    if (inactive) {
      // Tailwind 4 reports colors as oklch(), so resolve to sRGB through a canvas.
      const want = theme === 'dark' ? '156,163,175' : '75,85,99';
      const got = await el.evaluate((e) => ({
        color: (() => {
          const cx = document.createElement('canvas').getContext('2d');
          cx.fillStyle = getComputedStyle(e).color;
          cx.fillRect(0, 0, 1, 1);
          return cx.getImageData(0, 0, 1, 1).data.slice(0, 3).join(',');
        })(),
        faded: !!e.closest('.opacity-50'),
        disabledInput: !!e.parentElement.querySelector('input[disabled]'),
      }));
      expect(got.faded && got.disabledInput, `${name}: expected a deliberately inactive control (opacity-50 block with a disabled input)`).toBe(true);
      console.log(`CONTRAST ${theme} ${name} inactive color: ${got.color}`);
      // oklch -> sRGB rounding drifts a few units, so compare with a tolerance.
      const off = got.color.split(',').map((v, k) => Math.abs(Number(v) - Number(want.split(',')[k])));
      if (off.some((d) => d > 4)) failures.push(`${name} inactive label color rgb(${got.color}), want rgb(${want})`);
      continue;
    }
    // The card is translucent glass over a FIXED backdrop: pin the viewport
    // position so the pixels behind the text do not depend on the scroll offset.
    await el.evaluate((e) => e.scrollIntoView({ block: 'center' }));
    await page.evaluate(rAF2);
    await page.mouse.move(0, 0);
    const color = () => el.evaluate((e) => getComputedStyle(e).color);
    const resting = await color();
    const states = [['rest', await renderedContrast(page, el)]];
    if (hover) {
      await el.hover({ timeout: 5_000 });
      const hovered = await color();
      if (hovered === resting) {
        // Precondition for a hover measurement: the hover must change the color.
        // Recorded as a failure (not measured) so one run names every such element.
        failures.push(`${name} hover did not change the text color (${resting}), so a hover measurement would not be of the hover state`);
      } else {
        states.push(['hover', await renderedContrast(page, el)]);
      }
      await page.mouse.move(0, 0);
    }
    for (const [state, ratio] of states) {
      console.log(`CONTRAST ${theme} ${name} ${state}: ${ratio.toFixed(2)}`);
      if (ratio < threshold) failures.push(`${name} ${state} ${ratio.toFixed(2)}:1`);
    }
  }
  return failures;
}

// expectAA fails with every under-4.5 element the test collected.
function expectAA(theme, failures) {
  expect(failures, `${failures.length} element(s) painted under the required ratio (${theme}):\n${failures.join('\n')}`).toEqual([]);
}

const byText = (page, scope, tag, re) => page.locator(`${scope} ${tag}`).filter({ hasText: re });

// listLibraries returns the fixture's libraries; the API answers JSON null when
// there are none.
async function listLibraries(server) {
  const headers = { Cookie: `csrf_token=${server.csrfToken}; session=${server.sessionCookie}` };
  const resp = await fetch(`${server.baseURL}/api/v1/libraries`, { headers });
  expect(resp.ok, `listing libraries: ${resp.status}`).toBe(true);
  const body = await resp.json();
  return Array.isArray(body) ? body : (body?.libraries ?? []);
}

// ensureOneLibrary leaves the fixture with at least one library (a fresh temp dir
// when it has none), so a test that deletes libraries never leaks that state.
async function ensureOneLibrary(fx) {
  const { server } = fx;
  const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': server.csrfToken, Cookie: `csrf_token=${server.csrfToken}; session=${server.sessionCookie}` };
  if ((await listLibraries(server)).length === 0) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sw-muted-lib-'));
    const r = await fetch(`${server.baseURL}/api/v1/libraries`, { method: 'POST', headers, body: JSON.stringify({ name: 'Muted text fixture', path: dir, type: 'regular' }) });
    expect(r.ok, `fixture could not create a library: ${r.status}`).toBe(true);
  }
}

const settingsGroups = [
  {
    name: 'provider rows, priorities, platform and updates',
    sites: [
      ['settings.templ:1108 rate-limit label', (p) => byText(p, '[id^="provider-card-"]', 'span', /^[0-9.]+ req\/s/), { min: 5 }],
      ['settings.templ:1735 priority instructions', (p) => p.locator('[id^="priority-row-"] span:text-is("Drag to reorder. Click to enable/disable.")'), { min: 10 }],
      ['settings.templ:2386 NFO Disabled', (p) => p.locator('#section-platform span:text-is("Disabled")'), { min: 1 }],
      ['settings.templ:2924 Not yet checked', (p) => p.locator('#updates-latest-version span:text-is("Not yet checked")'), { min: 1 }],
    ],
  },
  {
    name: 'rules section',
    sites: [
      ['settings_sections.templ:906,923 rule sub-headings', (p) => p.locator('#section-rules h4.uppercase'), { min: 6 }],
      ['settings.templ:2415 catalogue link (icon)', (p) => p.locator('#section-rules a[target="_blank"][rel="noopener"][aria-label]'), { min: 20, hover: true, ratio: 3 }],
    ],
  },
  {
    name: 'MusicBrainz config panel',
    prep: async (page) => {
      await page.locator('[aria-controls="config-panel-musicbrainz"]').click();
      await expect(page.locator('#config-panel-musicbrainz')).toBeVisible();
      await page.locator('#config-form-musicbrainz input[name="server_type"][value="custom"]').check();
      await expect(page.locator('#custom-help-musicbrainz')).toBeVisible();
    },
    sites: [
      ['settings.templ:1304,1318 server hints', (p) => p.locator('#config-form-musicbrainz fieldset label > span.text-xs'), { min: 2 }],
      ['settings.templ:1384 custom-mirror help', (p) => p.locator('#custom-help-musicbrainz'), { min: 1 }],
      ['settings.templ:1390 OAuth heading', (p) => byText(p, '#config-form-musicbrainz', 'span', /^OAuth Credentials$/), { min: 1 }],
      ['settings.templ:1395 OAuth help', (p) => byText(p, '#config-form-musicbrainz', 'p', /^Required for submitting edits/), { min: 1 }],
      ['settings.templ:1400,1410 OAuth labels (inactive block)', (p) => byText(p, '#config-form-musicbrainz', 'label', /^Client (ID|Secret)$/), { min: 2, inactive: true }],
    ],
  },
  {
    name: 'Wikipedia config panel',
    prep: async (page) => {
      await page.locator('[aria-controls="config-panel-wikipedia"]').click();
      await expect(page.locator('#config-panel-wikipedia')).toBeVisible();
    },
    sites: [
      ['settings.templ:1445 verbosity description', (p) => byText(p, '#config-panel-wikipedia', 'p', /^Controls the level of detail/), { min: 1 }],
    ],
  },
  {
    name: 'connection path-mapping arrow',
    // The connection engages the write-back gate while it exists (see
    // helpers/seed-muted-text.js), so it lives only for this group.
    withConnection: true,
    prep: async (page) => {
      await page.locator('#section-connections [aria-controls^="features-"]').first().click();
      await expect(page.locator('[id^="path-mapping-block-"]').first()).toBeVisible();
    },
    sites: [
      ['settings_sections_next.templ:486 arrow glyph', (p) => p.locator('[id^="path-mapping-block-"] span[aria-hidden="true"]:text-is("→")'), { min: 1 }],
    ],
  },
  {
    name: 'libraries card with no libraries',
    // Removes every library on THIS throwaway server; `after` puts one back so
    // the later tests see the same state as the earlier ones.
    prep: async () => {
      const { server } = settingsFx;
      const headers = { 'X-CSRF-Token': server.csrfToken, Cookie: `csrf_token=${server.csrfToken}; session=${server.sessionCookie}` };
      for (const lib of await listLibraries(server)) {
        const r = await fetch(`${server.baseURL}/api/v1/libraries/${lib.id}`, { method: 'DELETE', headers });
        expect(r.ok, `fixture could not delete library ${lib.name}: ${r.status}`).toBe(true);
      }
    },
    reload: true,
    after: () => ensureOneLibrary(settingsFx),
    sites: [
      ['settings_sections.templ:436 no libraries', (p) => p.locator('#settings-no-libraries'), { min: 1 }],
    ],
  },
];

for (const theme of ['dark', 'light']) {
  for (const g of settingsGroups) {
    test(`settings: ${g.name} meets AA (${theme})`, async ({ browser }) => {
      if (g.reload) await g.prep();
      const connectionId = g.withConnection ? await addConnection(settingsFx.server) : null;
      let opened;
      try {
        opened = await open(browser, settingsFx, '/settings', theme);
      } catch (err) {
        if (connectionId) await removeConnection(settingsFx.server, connectionId);
        throw err;
      }
      const { context, page } = opened;
      try {
        if (g.prep && !g.reload) await g.prep(page);
        const failures = [];
        for (const [label, locate, opts] of g.sites) failures.push(...await measure(page, theme, label, locate(page), opts));
        expectAA(theme, failures);
      } finally {
        await context.close();
        if (connectionId) await removeConnection(settingsFx.server, connectionId);
        if (g.after) await g.after();
      }
    });
  }

  test(`register: optional marker meets AA (${theme})`, async ({ browser }) => {
    const { context, page } = await open(browser, settingsFx, `/register?code=${settingsFx.inviteCode}`, theme, false);
    try {
      await expect(page.locator('#reg-form-local'), 'the invite code did not open the registration form').toBeVisible();
      expectAA(theme, await measure(page, theme, 'register.templ:323 (optional)', page.locator('#reg-form-local label[for="reg-local-display"] span'), { min: 1 }));
    } finally {
      await context.close();
    }
  });
}

// Onboarding. The wizard is driven by its own goToStep(); the step is not a URL.
async function openWizard(browser, theme, step) {
  const o = await open(browser, onboardingFx, '/setup/wizard', theme);
  await o.page.evaluate((s) => window.goToStep(s), step);
  await expect(o.page.locator(`#wizard-step-${step}`)).toBeVisible();
  return o;
}

for (const theme of ['dark', 'light']) {
  test(`onboarding: no-libraries note, JS and server copies, meets AA (${theme})`, async ({ browser }) => {
    await ensureOneLibrary(onboardingFx);
    const { context, page } = await openWizard(browser, theme, 1);
    try {
      // JS copy (onboarding.templ:1059): remove the only library in the wizard.
      await expect(page.locator('#ob-library-list > [id^="ob-lib-"]'), 'want exactly one library row to remove').toHaveCount(1);
      await page.locator('#ob-library-list > [id^="ob-lib-"] button').click();
      const note = page.locator('#ob-no-libraries');
      await expect(note).toHaveCount(1);
      const jsClass = await note.getAttribute('class');
      const failures = await measure(page, theme, 'onboarding.templ:1059 JS copy', note, { min: 1 });

      // Server copy (onboarding.templ:209): reload with zero libraries.
      await page.reload();
      await page.waitForLoadState('load');
      await applyTheme(expect, page, theme);
      await page.evaluate(() => window.goToStep(1));
      await expect(page.locator('#wizard-step-1')).toBeVisible();
      const serverNote = page.locator('#ob-no-libraries');
      await expect(serverNote).toHaveCount(1);
      expect(await serverNote.getAttribute('class'), 'the JS-built copy must stay identical to the server-rendered one').toBe(jsClass);
      failures.push(...await measure(page, theme, 'onboarding.templ:209 server copy', serverNote, { min: 1 }));
      expectAA(theme, failures);
    } finally {
      await context.close();
    }
  });

  test(`onboarding: language skip note meets AA (${theme})`, async ({ browser }) => {
    const { context, page } = await openWizard(browser, theme, 2);
    try {
      expectAA(theme, await measure(page, theme, 'onboarding.templ:283 skip consequence', page.locator('#ob-lang-skip-btn ~ p'), { min: 1 }));
    } finally {
      await context.close();
    }
  });

  test(`onboarding: profile card Disabled meets AA (${theme})`, async ({ browser }) => {
    const { context, page } = await openWizard(browser, theme, 3);
    try {
      expectAA(theme, await measure(page, theme, 'onboarding.templ:1722 NFO Disabled', page.locator('#wizard-step-3 [data-profile-card] span:text-is("Disabled")'), { min: 1 }));
    } finally {
      await context.close();
    }
  });

  test(`onboarding: connection info buttons meet AA at rest and on hover (${theme})`, async ({ browser }) => {
    const { context, page } = await openWizard(browser, theme, 5);
    try {
      expectAA(theme, await measure(page, theme, 'onboarding.templ:1950 info button', page.locator('#wizard-step-5 button[aria-label][onclick*="toggleObConnectionInfo"]'), { min: 3, hover: true, ratio: 3 }));
    } finally {
      await context.close();
    }
  });
}
