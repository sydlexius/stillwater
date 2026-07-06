// nav-clips.mjs - record each hero SCREEN as its OWN clip (Playwright records one
// video per page, so a fresh page per screen = a clean per-screen clip). The
// Remotion side then stitches them back-to-back and dips to black at the EXACT
// clip boundaries - no guessing transition points in a continuous recording.
//
// OUTPUT: $OUT/clips/<NN-name>.webm  +  $OUT/clips/clips.json
//   clips.json = [{ name, file, durSec, captions:[{group, atSec}] }]  (atSec is the
//   time WITHIN the clip when that caption group should appear).
import { chromium, request as pwRequest } from 'playwright';
import { mkdirSync, readFileSync, renameSync, writeFileSync, rmSync } from 'node:fs';
import { join, dirname, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const MOCKS = join(HERE, 'mocks');
const PORT = process.env.HERO_PORT || '1991';
const BASE = `http://127.0.0.1:${PORT}`;
const ARTIST = process.env.HERO_ARTIST || 'c14e15f5-4ff4-4415-b2ea-75de8cb4be57';
const OUT = process.env.HERO_OUT || '/tmp/hero-1756/clips';
const RAW = join(OUT, 'raw');
const USER = process.env.HERO_ADMIN_USER || 'herofixture-admin';
const PASS = process.env.HERO_ADMIN_PASS || 'herofixture-pw';
const VIEWPORT = { width: 1600, height: 900 };
const mockHTML = (n) => readFileSync(join(MOCKS, n), 'utf8');
const log = (m) => console.log(`[nav-clips] ${m}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Start from an empty RAW dir so a stale .webm from an interrupted prior run
// can never be mistaken for the current recording. Guard the recursive delete
// against an unset/misconfigured RAW so a bad HERO_OUT can't wipe an unintended
// tree (RAW is always <OUT>/raw by construction).
if (!RAW || RAW === '/' || RAW === '.' || !RAW.endsWith(`${sep}raw`)) {
  throw new Error(`refusing to rmSync unsafe RAW path: ${RAW}`);
}
rmSync(RAW, { recursive: true, force: true });
mkdirSync(RAW, { recursive: true });

const api = await pwRequest.newContext({ baseURL: BASE });
const health = await api.get('/api/v1/health');
if (!health.ok()) throw new Error(`fixture not healthy on ${BASE}`);
const csrf = (health.headers()['set-cookie'] || '').match(/csrf_token=([^;]+)/)?.[1] || '';
const login = await api.post('/api/v1/auth/login', { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, Cookie: `csrf_token=${csrf}` }, data: JSON.stringify({ username: USER, password: PASS }) });
if (!login.ok()) throw new Error(`login failed: ${login.status()}`);
const session = (login.headers()['set-cookie'] || '').match(/session=([^;]+)/)?.[1];
if (!session) throw new Error('session cookie missing after login');
log('authenticated');

const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({
  viewport: VIEWPORT, deviceScaleFactor: 2, colorScheme: 'dark', baseURL: BASE,
  recordVideo: { dir: RAW, size: VIEWPORT },
});
// Larger INVERTED (black) synthetic cursor + click ripple.
await context.addInitScript(() => {
  const install = () => {
    if (document.getElementById('__hero_cursor')) return;
    const c = document.createElement('div');
    c.id = '__hero_cursor';
    c.style.cssText = [
      'position:fixed', 'left:0', 'top:0', 'width:34px', 'height:34px',
      'margin:-3px 0 0 -3px', 'z-index:2147483647', 'pointer-events:none',
      'transition:transform 40ms linear',
      'background:no-repeat center/contain url("data:image/svg+xml,' +
      encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M4 2l6 15 2.2-6.2L18.4 8 4 2z" fill="black" stroke="white" stroke-width="1.6" stroke-linejoin="round"/></svg>') +
      '")', 'filter:drop-shadow(0 1px 3px rgba(0,0,0,.55)) drop-shadow(0 0 2px rgba(255,255,255,.4))',
    ].join(';');
    document.documentElement.appendChild(c);
    window.addEventListener('mousemove', (e) => { c.style.transform = `translate(${e.clientX}px, ${e.clientY}px)`; }, true);
    // Expanding "pond" ripple on every click: two staggered brand-blue rings.
    // A DOUBLE requestAnimationFrame is required so the browser paints the start
    // state (scale 0.12 / opacity 0.95) before the transition kicks off - a single
    // rAF can fire pre-paint and the ring snaps straight to the end (no visible
    // expansion), which is why some earlier clicks showed no ripple.
    window.__heroRipple = (x, y) => {
      const ring = (delay, size, dur) => setTimeout(() => {
        const r = document.createElement('div');
        r.style.cssText = [
          'position:fixed', `left:${x}px`, `top:${y}px`,
          `width:${size}px`, `height:${size}px`, `margin:${-size / 2}px 0 0 ${-size / 2}px`,
          'border-radius:50%', 'z-index:2147483646', 'pointer-events:none',
          'border:3px solid rgba(191,219,254,0.95)',
          'background:radial-gradient(circle, rgba(191,219,254,0.30) 0%, rgba(147,197,253,0.08) 60%, transparent 72%)',
          'box-shadow:0 0 16px rgba(147,197,253,0.75)',
          'transform:scale(0.12)', 'opacity:0.95',
          `transition:transform ${dur}ms cubic-bezier(0.22,0.61,0.36,1),opacity ${dur}ms ease-out`,
        ].join(';');
        document.documentElement.appendChild(r);
        requestAnimationFrame(() => requestAnimationFrame(() => { r.style.transform = 'scale(1)'; r.style.opacity = '0'; }));
        setTimeout(() => r.remove(), dur + 80);
      }, delay);
      ring(0, 120, 620);   // leading ring
      ring(120, 156, 660); // trailing ring -> the "pond" spread
    };
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', install); else install();
  try { sessionStorage.setItem('sw_conflict_clean_dismissed', '1'); } catch (e) {}
});
await context.addCookies([
  { name: 'session', value: session, url: BASE },
  { name: 'csrf_token', value: csrf, url: BASE },
  { name: 'sw_ux', value: 'next', url: BASE },
]);
await context.route(/\/api\/v1\/artists\/[^/]+\/refresh(\?.*)?$/, (route) =>
  route.request().method() === 'POST' ? route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('refresh-metadata.html') }) : route.continue());
await context.route(/\/api\/v1\/artists\/[^/]+\/images\/(websearch|search)(\?.*)?$/, (route) =>
  route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('images-search-thumb.html') }));

const clips = [];

// Run one screen as its own page/clip. `body(page, markFn)` performs the screen;
// markFn(group) stamps a caption-group time within the clip.
const recordClip = async (name, body) => {
  const page = await context.newPage();
  const cursor = { x: VIEWPORT.width / 2, y: VIEWPORT.height / 2 };
  const t0 = Date.now();
  const captions = [];
  const mark = (group) => captions.push({ group, atSec: +((Date.now() - t0) / 1000).toFixed(2) });
  // contentStart = when the FIRST navigation settles (the lead blank/load before
  // this is trimmed off the clip so the stitch fades in on real content).
  let contentStart = 0; let gotFirst = false;
  const goto = async (path) => {
    await page.goto(path, { waitUntil: 'domcontentloaded' });
    await page.waitForLoadState('load').catch(() => {});
    await sleep(700);
    await page.keyboard.press('Escape').catch(() => {});
    await sleep(300);
    if (!gotFirst) { gotFirst = true; contentStart = +((Date.now() - t0) / 1000).toFixed(2); }
  };
  const moveClick = async (selector, { click = true, settle = 350 } = {}) => {
    const el = typeof selector === 'string' ? page.locator(selector).first() : selector;
    await el.evaluate((e) => e.scrollIntoView({ block: 'center', behavior: 'smooth' })).catch(() => {});
    await sleep(500);
    const box = await el.boundingBox(); if (!box) throw new Error(`no box for ${selector}`);
    const tx = box.x + box.width / 2, ty = box.y + box.height / 2;
    for (let i = 1; i <= 22; i++) { const p = i / 22, e = p < 0.5 ? 2 * p * p : 1 - Math.pow(-2 * p + 2, 2) / 2; await page.mouse.move(cursor.x + (tx - cursor.x) * e, cursor.y + (ty - cursor.y) * e); await sleep(12); }
    cursor.x = tx; cursor.y = ty;
    if (click) {
      await page.evaluate(([x, y]) => window.__heroRipple?.(x, y), [tx, ty]);
      await el.click({ timeout: 2500 }).catch(() => {});
      // Hold long enough for BOTH pond rings to fully expand + fade in-frame
      // (trailing ring starts +120ms, runs 660ms) on EVERY click, regardless of
      // what the click triggers next.
      await sleep(Math.max(settle, 820));
    } else {
      await sleep(settle);
    }
  };
  const feature = async (selector, settle = 1000) => {
    const el = typeof selector === 'string' ? page.locator(selector).first() : selector;
    await el.evaluate((e) => e.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'smooth' })).catch(() => {});
    await sleep(settle);
  };
  const tryStep = async (label, fn) => { try { await fn(); } catch (e) { console.warn(`[nav-clips] MISS ${label}: ${e.message}`); } };

  await body({ page, mark, goto, moveClick, feature, tryStep });

  const durSec = +((Date.now() - t0) / 1000).toFixed(2);
  // Playwright exposes this page's exact recording path; close() finalizes it.
  // Using it directly avoids a RAW directory scan that could pick the wrong file.
  const video = page.video();
  await page.close(); // finalizes the video
  const src = video ? await video.path() : null;
  if (!src) throw new Error(`no recorded video for clip ${name}`);
  const idx = String(clips.length + 1).padStart(2, '0');
  const dest = join(OUT, `${idx}-${name}.webm`);
  renameSync(src, dest);
  clips.push({ name, file: dest, durSec, contentStart, captions });
  log(`clip ${idx}-${name}: ${durSec}s (content@${contentStart}s), ${captions.length} caption group(s)`);
};

// ===== SCREENS (each its own clip) =====
await recordClip('dashboard', async ({ mark, goto }) => {
  mark('dashboard'); await goto('/next/'); await sleep(3000);
});

await recordClip('artists-grid', async ({ page, mark, goto, moveClick, tryStep }) => {
  await goto('/next/artists'); await sleep(600); mark('artists-grid');
  await tryStep('grid view', () => moveClick('button[data-view="grid"]'));
  // FLICKER FIX: the grid lazy-loads poster art, which pops in ~2s AFTER the
  // toggle click and reads as a phantom second click. Block until every visible
  // in-viewport image is fully decoded so the pop-in lands right at the click,
  // then hold on a fully-rendered grid.
  await tryStep('await posters', () => page.waitForFunction(() => {
    const imgs = Array.from(document.images).filter((im) => {
      const r = im.getBoundingClientRect();
      return r.width > 4 && r.height > 4 && r.bottom > 0 && r.top < window.innerHeight;
    });
    return imgs.length > 0 && imgs.every((im) => im.complete && im.naturalWidth > 0);
  }, { timeout: 6000 }));
  await sleep(2600);
});

await recordClip('artist-detail', async ({ page, mark, goto, moveClick, feature, tryStep }) => {
  await goto(`/next/artists/${ARTIST}`); await sleep(1500); mark('artist-detail'); await sleep(1500);
  mark('metadata');
  await tryStep('refresh', () => moveClick('#next-hero-refresh-btn'));
  await tryStep('ensure panel', async () => { const f = await page.locator('#refresh-panel :text("Metadata Refreshed")').count(); if (!f) await page.evaluate((id) => window.htmx?.ajax('POST', `/api/v1/artists/${id}/refresh`, { target: '#refresh-panel', swap: 'innerHTML' }), ARTIST); });
  await tryStep('feature panel', () => feature('#refresh-panel'));
  await sleep(1200);
  mark('edit');
  await tryStep('edit', () => moveClick(page.getByRole('button', { name: /edit all|edit/i }).first()));
  await sleep(900);
  await tryStep('feature lock', () => feature('.field-lock-toggle', 800));
  await sleep(1200);
  mark('images');
  await tryStep('open modal', () => moveClick('[data-sw-artwork-open]'));
  await page.locator('.sw-artwork-modal-surface').first().waitFor({ timeout: 4000 }).catch(() => {});
  await page.locator('#image-results img').first().waitFor({ timeout: 4000 }).catch(() => {});
  await tryStep('feature primary', () => feature('.sw-artwork-modal-surface', 1400));
  await tryStep('feature grid', () => feature('#image-results', 1600));
  await sleep(1400);
});

await recordClip('dashboard-loop', async ({ mark, goto }) => {
  mark('dashboard-loop'); await goto('/next/'); await sleep(3000);
});

writeFileSync(join(OUT, 'clips.json'), JSON.stringify(clips.map((c) => ({ ...c, file: c.file })), null, 2));
log(`clips manifest: ${join(OUT, 'clips.json')} (${clips.length} clips)`);

await context.close();
await browser.close();
await api.dispose();
