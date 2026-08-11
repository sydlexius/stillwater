// screenshots.mjs - capture clean, caption-free PNG screenshots of the promoted
// `/next` UI for outward-facing listings (the Unraid Community Applications
// template, docs pages, a README).
//
// WHY THIS EXISTS SEPARATELY FROM nav-clips.mjs
//
// The committed hero-static.png is a VIDEO POSTER FRAME, not a screenshot: it
// carries the burned-in kinetic captions and the synthetic cursor that
// HeroStitched composites downstream, which read as rendering artifacts once the
// image is shown outside a click-to-play player. This script stops before that
// pipeline -- no Remotion, no ffmpeg, no Node toolchain beyond Playwright -- and
// emits stills of the same screens.
//
// It shares nav-clips.mjs's fixture, auth, and route-mocks on purpose: the
// privacy properties are the whole point and must not be re-derived. The
// fixture is a purpose-built public-domain classical library, and every
// provider endpoint that would return real (keyed, copyrighted) artwork is
// intercepted and answered with the committed PD mocks. Nothing from a private
// library can appear in the output, by construction rather than by discipline.
//
// PREREQ: the fixture server must already be running --
//   HERO_PORT=1991 HERO_DIR=/tmp/hero-1756 docs/hero/seed-fixture.sh
//
// USAGE
//   node docs/hero/screenshots.mjs                 # all screens -> docs/hero/screenshots/
//   HERO_SHOT_OUT=/tmp/shots node docs/hero/screenshots.mjs
//   HERO_SHOT_FULLPAGE=1 node docs/hero/screenshots.mjs   # full scroll height
//
// OUTPUT: <out>/<name>.png at VIEWPORT x deviceScaleFactor:2 (3200x1800 by
// default), which is comfortably above what a CA listing or a docs page needs
// and downscales cleanly.
import { chromium, request as pwRequest } from 'playwright';
import { mkdirSync, readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const MOCKS = join(HERE, 'mocks');
const PORT = process.env.HERO_PORT || '1991';
const BASE = `http://127.0.0.1:${PORT}`;
const ARTIST = process.env.HERO_ARTIST || 'c14e15f5-4ff4-4415-b2ea-75de8cb4be57';
const OUT = process.env.HERO_SHOT_OUT || join(HERE, 'screenshots');
const USER = process.env.HERO_ADMIN_USER || 'herofixture-admin';
const PASS = process.env.HERO_ADMIN_PASS || 'herofixture-pw';
const FULL = process.env.HERO_SHOT_FULLPAGE === '1';
const VIEWPORT = { width: 1600, height: 900 };

const mockHTML = (n) => readFileSync(join(MOCKS, n), 'utf8');
const log = (m) => console.log(`[screenshots] ${m}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

mkdirSync(OUT, { recursive: true });

// Authenticate over the API first and reuse the cookies in the browser context,
// exactly as nav-clips.mjs does: it keeps a login form out of every capture and
// fails loudly here if the fixture is not up, rather than silently screenshotting
// a redirect to /login.
const api = await pwRequest.newContext({ baseURL: BASE });
const health = await api.get('/api/v1/health');
if (!health.ok()) {
  throw new Error(`fixture not healthy on ${BASE} -- run docs/hero/seed-fixture.sh first`);
}
const csrf = (health.headers()['set-cookie'] || '').match(/csrf_token=([^;]+)/)?.[1] || '';
const login = await api.post('/api/v1/auth/login', {
  headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, Cookie: `csrf_token=${csrf}` },
  data: JSON.stringify({ username: USER, password: PASS }),
});
if (!login.ok()) throw new Error(`login failed: ${login.status()}`);
const session = (login.headers()['set-cookie'] || '').match(/session=([^;]+)/)?.[1];
if (!session) throw new Error('session cookie missing after login');
log('authenticated');

const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({
  viewport: VIEWPORT, deviceScaleFactor: 2, colorScheme: 'dark', baseURL: BASE,
});

// NO synthetic cursor and NO click ripple are installed here (nav-clips.mjs adds
// both for the video). A still frame wants neither: a frozen cursor mid-screen
// reads as a smudge, and a half-expanded ripple reads as a rendering bug.
await context.addInitScript(() => {
  try { sessionStorage.setItem('sw_conflict_clean_dismissed', '1'); } catch { /* first-visit banner is best-effort */ }
});
await context.addCookies([
  { name: 'session', value: session, url: BASE },
  { name: 'csrf_token', value: csrf, url: BASE },
  { name: 'sw_ux', value: 'next', url: BASE },
]);
// Same route-mocks as the video path: the real image/metadata providers are
// keyed and return copyrighted artwork, so they are answered with the committed
// public-domain fixtures instead. Never remove these -- they are what makes the
// output publishable.
await context.route(/\/api\/v1\/artists\/[^/]+\/refresh(\?.*)?$/, (route) =>
  route.request().method() === 'POST'
    ? route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('refresh-metadata.html') })
    : route.continue());
await context.route(/\/api\/v1\/artists\/[^/]+\/images\/(websearch|search)(\?.*)?$/, (route) =>
  route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('images-search-thumb.html') }));

const shot = async (name, body) => {
  const page = await context.newPage();
  const goto = async (path) => {
    await page.goto(path, { waitUntil: 'domcontentloaded' });
    await page.waitForLoadState('load').catch(() => {});
    await sleep(700);
    // Dismiss any transient overlay so it cannot sit half-open in the still.
    await page.keyboard.press('Escape').catch(() => {});
    await sleep(300);
  };
  // Block until every in-viewport image is DECODED, not merely loaded. Without
  // this the grid's lazy-loaded poster art can still be blank at capture time,
  // which is invisible in a video (it pops in a frame later) but permanent in a
  // still.
  //
  // Two steps, because `complete` is not the property that matters: it goes true
  // when the FETCH finishes, which can still precede rasterization. So wait for
  // load first (a cheap synchronous predicate `waitForFunction` can poll), then
  // await decode() on each image, which resolves only once the frame is ready to
  // paint. A rejected decode is swallowed per-image: a single broken asset should
  // degrade that one image, not abort the capture.
  const settleImages = async (timeout = 6000) => {
    try {
      await page.waitForFunction(() => {
        const imgs = Array.from(document.images).filter((im) => {
          const r = im.getBoundingClientRect();
          return r.width > 4 && r.height > 4 && r.bottom > 0 && r.top < window.innerHeight;
        });
        return imgs.every((im) => im.complete && im.naturalWidth > 0);
      }, { timeout });
      await page.evaluate(async () => {
        const imgs = Array.from(document.images).filter((im) => {
          const r = im.getBoundingClientRect();
          return r.width > 4 && r.height > 4 && r.bottom > 0 && r.top < window.innerHeight;
        });
        await Promise.all(imgs.map((im) => (im.decode ? im.decode().catch(() => {}) : null)));
      });
    } catch {
      log(`WARN ${name}: images did not all settle; capturing anyway`);
    }
  };
  const tryStep = async (label, fn) => {
    try { await fn(); } catch (e) { log(`MISS ${name}/${label}: ${e.message}`); }
  };

  await body({ page, goto, settleImages, tryStep });

  await page.waitForLoadState('networkidle').catch(() => {});
  await settleImages(3000);
  const file = join(OUT, `${name}.png`);
  await page.screenshot({ path: file, fullPage: FULL });
  await page.close();
  log(`wrote ${file}`);
};

// ===== SCREENS =====
// Each is a still of a settled screen. Interactions exist only to REACH the
// state worth showing, never to demonstrate motion.

await shot('dashboard', async ({ goto, settleImages }) => {
  await goto('/next/');
  await settleImages();
  await sleep(600);
});

await shot('artists-grid', async ({ page, goto, settleImages, tryStep }) => {
  await goto('/next/artists');
  // Grid rather than the default list: poster art is what makes this screen
  // worth showing at all.
  await tryStep('grid view', () => page.locator('button[data-view="grid"]').first().click({ timeout: 3000 }));
  await settleImages();
  await sleep(600);
});

await shot('artist-detail', async ({ goto, settleImages }) => {
  await goto(`/next/artists/${ARTIST}`);
  await settleImages();
  await sleep(900);
});

await shot('artwork-candidates', async ({ page, goto, settleImages, tryStep }) => {
  await goto(`/next/artists/${ARTIST}`);
  await tryStep('open modal', () => page.locator('[data-sw-artwork-open]').first().click({ timeout: 4000 }));
  await page.locator('.sw-artwork-modal-surface').first().waitFor({ timeout: 4000 }).catch(() => {});
  await page.locator('#image-results img').first().waitFor({ timeout: 4000 }).catch(() => {});
  await settleImages();
  await sleep(900);
});

await browser.close();
await api.dispose();
log(`done -- ${OUT}`);
