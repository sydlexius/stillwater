// seed-muted-text.js - throwaway servers for muted-text-contrast.spec.js (#3474).
//
// The shared a11y server is bootstrapped with onboarding already complete, so
// /setup/wizard redirects away from it, and it has multi_user off, so /register
// is a 404. Flipping either on the shared server would change what every later
// spec sees. This spec therefore boots its OWN servers (the base-path-server
// pattern): nothing it seeds is visible to, or outlives, any other spec.
//
//   startSettingsFixture()    admin + onboarding complete + multi-user on + one
//                             invite code, for /settings and /register.
//   addConnection()/removeConnection()
//                             a platform connection (skip_test, no network). It is
//                             NOT part of the fixture's base state: a media-server
//                             connection engages the write-back conflict gate,
//                             which greys out and disables the rule cards
//                             (opacity .55, pointer-events none) and would corrupt
//                             every other settings measurement. The one test that
//                             needs it adds it and removes it in a finally.
//   startOnboardingFixture()  admin created and logged in, onboarding NOT marked
//                             complete, so /setup/wizard serves the wizard.
//   startPagesFixture()       a library with a conflicting duplicate pair and one
//                             lone artist, for the duplicates, artist-detail and
//                             error pages (slice 3b-1).
//   addIgnoredGroup()/removeIgnoredGroup()
//                             a persisted ignore with no group key or reason, so
//                             the manage-ignored page renders its placeholders.
//   addPlatformArtist()/removePlatformArtist()
//                             a connection to a loopback fake Emby whose one music
//                             library and one folder-less artist are imported, so the
//                             duplicates page renders its "platform only" row and the
//                             discover list renders "(already imported)". It joins the
//                             fixture's duplicate group as a THIRD member (which breaks
//                             the merge dialog's two-radio expectation), so the one
//                             test that needs it removes it in a finally.

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

import { startBasePathServer } from './base-path-server.js';

const ADMIN_USER = 'ci-a11y-muted-admin';
const ADMIN_PASS = 'ci-a11y-muted-ephemeral-pw';

function headers(csrfToken, session) {
  const cookie = session ? `csrf_token=${csrfToken}; session=${session}` : `csrf_token=${csrfToken}`;
  return { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken, Cookie: cookie };
}

async function call(server, method, path, body, session) {
  const resp = await fetch(`${server.baseURL}${path}`, {
    method,
    headers: headers(server.csrfToken, session ?? server.sessionCookie),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`seed-muted-text: ${method} ${path} -> ${resp.status} ${await resp.text()}`);
  return resp;
}

export async function startSettingsFixture() {
  const server = await startBasePathServer('', { env: { SW_UX: 'next' } });
  try {
    await call(server, 'PUT', '/api/v1/settings', { 'multi_user.enabled': 'true' });
    const invite = await (await call(server, 'POST', '/api/v1/users/invites', { role: 'operator', expires_in: '1h' })).json();
    return { server, inviteCode: invite.code };
  } catch (err) {
    server.stop();
    throw err;
  }
}

// addConnection stores an Emby connection without contacting it (skip_test) and
// returns its id.
export async function addConnection(server) {
  const resp = await call(server, 'POST', '/api/v1/connections', {
    name: 'Muted text fixture', type: 'emby', url: 'http://127.0.0.1:9', api_key: 'fixture', enabled: true, skip_test: true,
  });
  return (await resp.json()).id;
}

export async function removeConnection(server, id) {
  await call(server, 'DELETE', `/api/v1/connections/${id}`);
}

export async function startOnboardingFixture() {
  const server = await startBasePathServer('', { skipBootstrap: true, env: { SW_UX: 'next' } });
  try {
    const health = await fetch(`${server.baseURL}/api/v1/health`);
    if (!health.ok) throw new Error(`seed-muted-text: GET /api/v1/health -> ${health.status}`);
    const csrfToken = (health.headers.get('set-cookie') || '').match(/csrf_token=([^;]+)/)?.[1] || '';
    if (!csrfToken) throw new Error('seed-muted-text: health response carried no csrf_token cookie');
    server.csrfToken = csrfToken;
    await call(server, 'POST', '/api/v1/auth/setup', { username: ADMIN_USER, password: ADMIN_PASS }, null);
    const login = await call(server, 'POST', '/api/v1/auth/login', { username: ADMIN_USER, password: ADMIN_PASS }, null);
    const session = (login.headers.get('set-cookie') || '').match(/session=([^;]+)/)?.[1];
    if (!session) throw new Error('seed-muted-text: login response carried no session cookie');
    server.sessionCookie = session;
    return { server };
  } catch (err) {
    server.stop();
    throw err;
  }
}

// Names the pages spec asserts on. The duplicate pair shares one name (so the
// name-key detector groups them) but carries DIFFERENT disambiguations (so the
// group is a "conflicting duplicate group") and only one of them has an MBID (so
// the other renders the "None" placeholder).
export const PAGES = {
  libraryName: 'Muted pages fixture',
  dupName: 'Muted Duplicate Fixture',
  soloName: 'Muted Solo Fixture',
};
const MBID = '11111111-2222-4333-8444-555555555555';

const xml = (v) => v.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
function writeArtist(root, dir, { name, disambiguation, mbid }) {
  fs.mkdirSync(path.join(root, dir), { recursive: true });
  const body = [`<name>${xml(name)}</name>`];
  if (disambiguation) body.push(`<disambiguation>${xml(disambiguation)}</disambiguation>`);
  if (mbid) body.push(`<musicbrainzartistid>${mbid}</musicbrainzartistid>`);
  fs.writeFileSync(path.join(root, dir, 'artist.nfo'),
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist>${body.join('')}</artist>\n`);
}

// startPagesFixture boots a throwaway server whose library holds a conflicting
// duplicate pair plus a lone artist, scans it, and PROVES the defining
// properties before returning (the page really lists one conflicting group with
// one MBID placeholder, and the lone artist has an id). The library lives in the
// server's temp dir, so server.stop() removes everything seeded here.
export async function startPagesFixture() {
  let libDir = '';
  const server = await startBasePathServer('', {
    env: { SW_UX: 'next' },
    seed: (tmp) => {
      libDir = path.join(tmp, 'pages-library');
      writeArtist(libDir, 'Muted Duplicate Fixture', { name: PAGES.dupName, disambiguation: 'alpha', mbid: MBID });
      writeArtist(libDir, 'Muted Duplicate Fixture (copy)', { name: PAGES.dupName, disambiguation: 'beta' });
      writeArtist(libDir, 'Muted Solo Fixture', { name: PAGES.soloName });
    },
  });
  try {
    await call(server, 'POST', '/api/v1/libraries', { name: PAGES.libraryName, path: libDir, type: 'regular' });
    await call(server, 'POST', '/api/v1/scanner/run');
    const deadline = Date.now() + 60_000;
    for (;;) {
      const st = await (await call(server, 'GET', '/api/v1/scanner/status')).json();
      if (st.status === 'completed' || st.status === 'idle') break;
      if (Date.now() > deadline) throw new Error('seed-muted-text: scan did not complete within 60s');
      await new Promise((r) => setTimeout(r, 250));
    }
    const html = await (await call(server, 'GET', '/reports/duplicates')).text();
    const groups = (html.match(/<div[^>]*data-duplicate-group[ >]/g) || []).length;
    if (groups !== 1) throw new Error(`seed-muted-text: want exactly 1 duplicate group, found ${groups}`);
    if (!html.includes('Disambiguation Conflict')) throw new Error('seed-muted-text: the duplicate group is not flagged as a disambiguation conflict');
    const none = (html.match(/<span class="[^"]*">None<\/span>/g) || []).length;
    if (none < 1) throw new Error('seed-muted-text: no member renders the "None" MBID placeholder');
    const list = await (await call(server, 'GET', '/api/v1/artists?page_size=100')).json();
    const solo = (Array.isArray(list) ? list : list.artists || []).find((a) => a.name === PAGES.soloName);
    if (!solo) throw new Error('seed-muted-text: the lone artist was not imported');
    // One manual edit gives the lone artist a field-history entry, which is what
    // makes the per-field history (clock) icon render in the edit cluster.
    await call(server, 'PATCH', `/api/v1/artists/${solo.id}/fields/origin`, { value: 'Fixtureland' });
    return { server, soloId: solo.id };
  } catch (err) {
    server.stop();
    throw err;
  }
}

// addIgnoredGroup persists an ignore with NO group key and NO reason (what an
// ignore recorded before the display context existed looks like), so the
// manage-ignored page renders both muted placeholders. The member ids need not
// exist: the signature is computed from them. Returns the ignore's row id.
export async function addIgnoredGroup(server) {
  await call(server, 'POST', '/api/v1/artists/duplicates/ignore', { member_ids: ['muted-fixture-a', 'muted-fixture-b'] });
  const rows = await (await call(server, 'GET', '/api/v1/artists/duplicates/ignored')).json();
  const list = rows.items || [];
  if (list.length !== 1) throw new Error(`seed-muted-text: want 1 ignored group, got ${JSON.stringify(rows)}`);
  return list[0].id;
}

export async function removeIgnoredGroup(server, id) {
  await call(server, 'DELETE', `/api/v1/artists/duplicates/ignored/${id}`);
}

// startLoopbackEmby answers just enough of the Emby REST surface for a connection
// test, a library listing and an artist import: one music library and one album
// artist. The artist carries a leading "The " so its identity key matches the
// fixture's duplicate pair (the normalizer strips a leading article) while the
// name is not LOWER()-equal to it, so the import creates a NEW folder-less row.
// helpers/seed-platform-backdrop-duplicates.js has a startFakeEmby too, but it
// answers an EMPTY library list and no artists, so it cannot serve this.
function startLoopbackEmby() {
  const server = http.createServer((req, res) => {
    const u = new URL(req.url, 'http://127.0.0.1');
    const json = (body) => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(body)); };
    if (u.pathname === '/System/Info') return json({ ServerName: 'muted-fake-emby', Version: '4.0.0', Id: 'muted-fake' });
    if (u.pathname === '/Users') return json([{ Id: 'muted-user-id', Name: 'fake' }]);
    if (u.pathname === '/Library/VirtualFolders') {
      return json([{ Name: 'Muted Platform Music', Locations: [], CollectionType: 'music', ItemId: 'muted-lib-1', LibraryOptions: {} }]);
    }
    if (u.pathname === '/Artists/AlbumArtists') {
      return json({ Items: [{ Name: `The ${PAGES.dupName}`, Id: 'muted-platform-artist-1' }], TotalRecordCount: 1 });
    }
    res.writeHead(404, { 'Content-Type': 'application/json' }).end('{}');
    return undefined;
  });
  return new Promise((resolve, reject) => {
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => resolve({ server, url: `http://127.0.0.1:${server.address().port}` }));
  });
}

// addPlatformArtist connects the loopback Emby, imports its library and populates
// it, then PROVES the defining property before returning: the platform artist
// exists and has no folder. Returns what removePlatformArtist needs.
export async function addPlatformArtist(server) {
  const fake = await startLoopbackEmby();
  const created = { fake, connectionId: '', libraryId: '', artistId: '' };
  try {
    const conn = await (await call(server, 'POST', '/api/v1/connections', { name: 'Muted platform fixture', type: 'emby', url: fake.url, api_key: 'fixture', enabled: true })).json();
    created.connectionId = conn.id;
    await call(server, 'POST', `/api/v1/connections/${conn.id}/libraries/import`, { libraries: [{ external_id: 'muted-lib-1', name: 'Muted Platform Music' }] });
    const libs = await (await call(server, 'GET', '/api/v1/libraries')).json();
    const lib = (Array.isArray(libs) ? libs : libs.libraries || []).find((l) => l.external_id === 'muted-lib-1');
    if (!lib) throw new Error('seed-muted-text: the platform library was not imported');
    created.libraryId = lib.id;
    await call(server, 'POST', `/api/v1/connections/${conn.id}/libraries/${lib.id}/populate`);
    const platformName = `The ${PAGES.dupName}`;
    const deadline = Date.now() + 30_000;
    for (;;) {
      const list = await (await call(server, 'GET', '/api/v1/artists?page_size=100')).json();
      const hit = (Array.isArray(list) ? list : list.artists || []).find((a) => a.name === platformName);
      if (hit) {
        if (hit.path) throw new Error(`seed-muted-text: the platform artist must have no folder, got ${hit.path}`);
        created.artistId = hit.id;
        return created;
      }
      if (Date.now() > deadline) throw new Error('seed-muted-text: the platform artist was not created within 30s');
      await new Promise((r) => setTimeout(r, 500));
    }
  } catch (err) {
    await removePlatformArtist(server, created).catch(() => {});
    throw err;
  }
}

// removePlatformArtist undoes addPlatformArtist: deleting the library together with
// its artists drops the folder-less row, then the connection and the listener go.
export async function removePlatformArtist(server, created) {
  try {
    if (created.libraryId) await call(server, 'DELETE', `/api/v1/libraries/${created.libraryId}?deleteArtists=true`);
    if (created.connectionId) await call(server, 'DELETE', `/api/v1/connections/${created.connectionId}`);
  } finally {
    created.fake.server.close();
  }
}
