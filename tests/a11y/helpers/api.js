// api.js - shared HTTP + fixture-library helpers for the a11y seed-* fixture
// scripts. Extracted from three near-identical copies that had drifted apart
// in seed-backdrop-duplicates.js, seed-blast-radius.js, and seed-nfo-mbid.js.
//
// Pure refactor: every function here behaves identically to the copy it
// replaces for every existing caller. Where the copies disagreed (the fixture
// library name), that became a parameter instead of a module constant, since
// baking one seeder's name into a shared function would silently change what
// the others create.

export const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

/**
 * apiFetch issues an authenticated, CSRF-bearing request against the
 * ephemeral server. Playwright's APIRequestContext carries the session
 * cookie from global-setup; the CSRF token is read back off that context's
 * cookie jar because state-changing routes require the header to match.
 */
export async function apiFetch(request, method, url, body) {
  const state = await request.storageState();
  const csrf = (state.cookies.find(c => c.name === 'csrf_token') || {}).value || '';
  const opts = {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
  };
  if (body !== undefined) opts.data = JSON.stringify(body);
  return request.fetch(`${BASE_URL}${url}`, { method, ...opts });
}

/**
 * ensureLibrary points a fixture library named libraryName at dir and
 * returns its id, reusing a previous run's row when it already points where
 * this run expects.
 *
 * 409 means a previous run created it (the name is unique in the schema).
 * Reuse is only safe when the existing row still points at this run's
 * directory -- a stale row aimed at a deleted temp dir would scan nothing
 * and seed an empty fixture, which is the failure this helper exists to
 * prevent. Any other status is a real failure and still throws.
 */
export async function ensureLibrary(request, libraryName, dir) {
  const resp = await apiFetch(request, 'POST', '/api/v1/libraries', {
    name: libraryName, path: dir, type: 'regular',
  });
  if (resp.ok()) return (await resp.json()).id;

  if (resp.status() === 409) {
    const list = await request.fetch(`${BASE_URL}/api/v1/libraries`);
    if (list.ok()) {
      const body = await list.json();
      const libs = Array.isArray(body) ? body : (body.libraries || []);
      const existing = libs.find(l => l.name === libraryName);
      if (existing && existing.path === dir) return existing.id;
      if (existing) {
        throw new Error(
          `seed: fixture library exists but points at ${existing.path}, not ${dir}. `
          + 'Delete it (or use a fresh database) before re-seeding.',
        );
      }
    }
  }
  throw new Error(`seed: creating fixture library failed: ${resp.status()} ${await resp.text()}`);
}

/** runScan triggers a scan and waits for it to finish. */
export async function runScan(request) {
  const resp = await apiFetch(request, 'POST', '/api/v1/scanner/run');
  if (!resp.ok()) {
    throw new Error(`seed: scanner run failed: ${resp.status()} ${await resp.text()}`);
  }
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const st = await request.fetch(`${BASE_URL}/api/v1/scanner/status`);
    if (st.ok()) {
      const body = await st.json();
      if (body.status === 'completed' || body.status === 'idle') return;
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error('seed: scan did not complete within 60s');
}

/** artistIdsByName maps fixture artist names to their ids. */
export async function artistIdsByName(request, names) {
  const resp = await request.fetch(`${BASE_URL}/api/v1/artists?page_size=500`);
  if (!resp.ok()) {
    throw new Error(`seed: listing artists failed: ${resp.status()}`);
  }
  const body = await resp.json();
  const list = Array.isArray(body) ? body : (body.artists || []);
  const out = new Map();
  for (const a of list) {
    if (names.includes(a.name)) out.set(a.name, a.id);
  }
  return out;
}
