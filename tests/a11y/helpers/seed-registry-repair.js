// seed-registry-repair.js - own-server fixture for the image-registry repair
// banner specs (#2678): registry-repair-banner, registry-repair-run and
// registry-repair-locale.
//
// WHY A SERVER OF ITS OWN. The banner reads a CACHED detector
// (GET .../registry-repair/banner) that a background job fills 2 minutes after
// boot and every 12h after, so the shared harness server (empty database, empty
// library) can never be made to report needs_repair=true after the fact. The
// fixture boots a throwaway server (helpers/base-path-server.js) with
// SW_REGISTRY_REPAIR_CHECK_EVERY=2s, the detector-cadence test seam in
// cmd/stillwater/main.go (a TEST HOOK: refused on release builds, ignored
// outside [1s, 24h]), then builds the defect the detector looks for: an artist
// made by a real scan whose folder holds an image file with NO registry row.
// The image is written AFTER the scan because the scanner would otherwise
// register it.
//
// The fixture's defining property is asserted on the endpoint itself (needs_repair
// true, count > 0) before it is returned, so a page is never trusted first.
// The fixture can be repaired exactly once, so each caller boots its own.

import fs from 'node:fs';
import path from 'node:path';
import { expect } from 'playwright/test';

import { startBasePathServer } from './base-path-server.js';

export const BANNER_API = '/api/v1/reports/registry-repair/banner';

// A valid 1x1 PNG: the repair decodes every candidate, so it must be real.
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==',
  'base64',
);

// startRegistryRepairFixture boots the server and seeds the defect. Returns
// { server, fx, body, stop }: fx(method, path, body?, session?) issues an
// authenticated request, body is the asserted banner endpoint answer.
export async function startRegistryRepairFixture(basePath, artistName) {
  let tmpDir;
  const server = await startBasePathServer(basePath, {
    env: { SW_REGISTRY_REPAIR_CHECK_EVERY: '2s' },
    seed: (dir) => { tmpDir = dir; },
  });
  const stop = () => server.stop();
  try {
    const fx = (method, urlPath, body, session = server.sessionCookie) => fetch(`${server.baseURL}${urlPath}`, {
      method,
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': server.csrfToken,
        Cookie: `session=${session}; csrf_token=${server.csrfToken}`,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const lib = path.join(tmpDir, 'empty-library');
    const artistDir = path.join(lib, artistName);
    fs.mkdirSync(artistDir, { recursive: true });
    expect((await fx('POST', '/api/v1/libraries', { name: 'repair fixture', path: lib, type: 'regular' })).ok).toBe(true);
    expect((await fx('POST', '/api/v1/scanner/run')).ok).toBe(true);
    await expect.poll(async () => (await (await fx('GET', '/api/v1/scanner/status')).json()).status,
      { timeout: 30_000 }).toMatch(/completed|idle/);
    // Written after the scan: a file on disk with no registry row.
    fs.writeFileSync(path.join(artistDir, 'folder.png'), PNG_1X1);

    await expect.poll(async () => (await (await fx('GET', BANNER_API)).json()).needs_repair,
      { message: 'detector never reported needs_repair=true for the fixture', timeout: 30_000 }).toBe(true);
    const body = await (await fx('GET', BANNER_API)).json();
    expect(body.ok).toBe(true);
    expect(body.count).toBeGreaterThan(0);
    return { server, fx, body, stop };
  } catch (err) {
    stop();
    throw err;
  }
}
