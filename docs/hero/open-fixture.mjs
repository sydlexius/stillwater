// open-fixture.mjs - bring up a HEADED, logged-in browser on the PD hero fixture
// so the maintainer can walk the /next UX interactively. NOT a recording harness:
// it launches, logs in, installs the PD route mocks (so any image-search view
// stays copyright-safe - the live endpoints hit real keyed providers), opens
// Bach's detail page, and holds the window open until it's closed (or 60 min).
//
// USAGE: cd <worktree> && node docs/hero/open-fixture.mjs
//   HERO_START=/next/artists/<id> to change the opening page.
import { chromium, request as pwRequest } from 'playwright';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const MOCKS = join(HERE, 'mocks');
const PORT = process.env.HERO_PORT || '1991';
const BASE = `http://127.0.0.1:${PORT}`;
const ARTIST = process.env.HERO_ARTIST || 'c14e15f5-4ff4-4415-b2ea-75de8cb4be57';
const START = process.env.HERO_START || `/next/artists/${ARTIST}`;
const USER = process.env.HERO_ADMIN_USER || 'herofixture-admin';
const PASS = process.env.HERO_ADMIN_PASS || 'herofixture-pw';
const mockHTML = (n) => readFileSync(join(MOCKS, n), 'utf8');
const log = (m) => console.log(`[open-fixture] ${m}`);

const api = await pwRequest.newContext({ baseURL: BASE });
const health = await api.get('/api/v1/health');
if (!health.ok()) throw new Error(`fixture not healthy on ${BASE}: ${health.status()}`);
const csrf = (health.headers()['set-cookie'] || '').match(/csrf_token=([^;]+)/)?.[1] || '';
const login = await api.post('/api/v1/auth/login', {
  headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, Cookie: `csrf_token=${csrf}` },
  data: JSON.stringify({ username: USER, password: PASS }),
});
if (!login.ok()) throw new Error(`login failed: ${login.status()}`);
const session = (login.headers()['set-cookie'] || '').match(/session=([^;]+)/)?.[1];
log('authenticated');

// Taller window so ordinary /next pages need no scrollbar and the Manage-artwork
// modal shows Current Primary + the first candidate row without clipping. (The
// modal's full content is ~1646px, so browsing far down the candidate grid still
// scrolls by design; that is not an "unneeded" scrollbar.)
const browser = await chromium.launch({
  headless: false,
  args: ['--window-position=40,30', '--window-size=1480,1180'],
});
const context = await browser.newContext({ viewport: null, colorScheme: 'dark', baseURL: BASE });
await context.addCookies([
  { name: 'session', value: session, url: BASE },
  { name: 'csrf_token', value: csrf, url: BASE },
  { name: 'sw_ux', value: 'next', url: BASE },
]);
// PD-safe: keep image-search + refresh views on the PD fixtures (live endpoints
// hit real keyed providers = copyrighted). Mocks travel with the context.
await context.route(/\/api\/v1\/artists\/[^/]+\/refresh(\?.*)?$/, (route) =>
  route.request().method() === 'POST'
    ? route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('refresh-metadata.html') })
    : route.continue());
await context.route(/\/api\/v1\/artists\/[^/]+\/images\/(websearch|search)(\?.*)?$/, (route) =>
  route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: mockHTML('images-search-thumb.html') }));

const page = await context.newPage();
await page.goto(START, { waitUntil: 'domcontentloaded' });
log(`window up at (60,60), logged in, on ${START}`);
log('Image-search + refresh are PD-mocked (copyright-safe). Drive it freely.');
log('Close the browser window (or wait 60 min) to end this process.');

// Hold open until the browser is closed or 60 minutes elapse.
await new Promise((resolve) => {
  browser.on('disconnected', resolve);
  setTimeout(resolve, 60 * 60 * 1000);
});
await api.dispose().catch(() => {});
log('closed.');
