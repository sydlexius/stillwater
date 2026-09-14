// seed-websearch-unavailable.js - fixture for websearch-unavailable.spec.js
// (#3229).
//
// `make test-a11y` boots an empty DB/library with no reliable outbound
// network, so a real DuckDuckGo call would hang or 403 non-deterministically.
// Instead this uses internal/provider/injection.go's sanctioned
// SW_FORCE_PROVIDER_ERROR seam (already used by the Provider Failure Smoke
// job), wired into duckduckgo.Adapter.SearchImages. The Makefile's
// test-a11y target and ci.yml's "Start Stillwater (ephemeral)" step both set
// SW_FORCE_PROVIDER_ERROR=duckduckgo (hand-mirrored; see each site's
// comment); a release build refuses the var outright (main.go).
//
// PROVING THE SEAM, NOT A REAL OUTAGE (#3229 review, C1/M5): the first
// version of this fixture asserted only status==="unavailable", which also
// passes if the injection wiring is entirely absent and DuckDuckGo happens
// to be down live (it was, at review time) -- exactly the false pass that
// let ci.yml ship without the env var at all. The fix asserts the response
// header X-Stillwater-Websearch-Injected-Failure, set by
// handleWebImageSearch only when errors.Is(err, provider.ErrInjectedFailure)
// -- a live network error can never satisfy that. A header travels with the
// request under test; a server log file does not (this harness has no path
// to one it did not itself start).
//
// WHAT GETS SEEDED: one artist via a real library scan (no artist-create
// endpoint -- see seed-blast-radius.js); the DuckDuckGo provider toggled on;
// then a direct GET of the endpoint under test, asserting status,
// unavailable_providers, AND the injection header BEFORE returning the id --
// together the fixture's defining property, proven against the real server.

import path from 'node:path';
import os from 'node:os';
import fs from 'node:fs';
import {
  BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName,
} from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y websearch-unavailable fixture';
export const WEBSEARCH_UNAVAILABLE_ARTIST = 'Websearch Unavailable Fixture';

/**
 * seedWebSearchUnavailableArtist scans one artist, enables the DuckDuckGo web
 * search provider, and proves (via a direct API call) that a web image
 * search for that artist reports status="unavailable" with
 * unavailable_providers=["duckduckgo"] under this harness's injected
 * failure. Returns the artist's id.
 */
export async function seedWebSearchUnavailableArtist(request) {
  const port = process.env.SW_PORT || new URL(BASE_URL).port || 'default';
  const dir = path.join(os.tmpdir(), `sw-a11y-websearch-unavailable-${port}`);
  const artistDir = path.join(dir, WEBSEARCH_UNAVAILABLE_ARTIST);
  fs.mkdirSync(artistDir, { recursive: true });

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);

  const id = (await artistIdsByName(request, [WEBSEARCH_UNAVAILABLE_ARTIST])).get(WEBSEARCH_UNAVAILABLE_ARTIST);
  if (!id) throw new Error('seed: websearch-unavailable fixture artist did not appear after scan');

  const toggleResp = await apiFetch(request, 'PUT', '/api/v1/providers/websearch/duckduckgo/toggle', { enabled: true });
  if (!toggleResp.ok()) {
    throw new Error(`seed: enabling the DuckDuckGo web search provider failed: ${toggleResp.status()} ${await toggleResp.text()}`);
  }

  // Assert the fixture's defining property against the REAL endpoint under
  // test, not merely that the provider toggle succeeded -- a regression that
  // silently disabled SW_FORCE_PROVIDER_ERROR wiring (or left the provider
  // reporting ok on empty results, the OTHER shape this response can take)
  // must fail here, loudly, rather than let the spec discover an empty page
  // and misreport it as a rendering defect.
  const searchResp = await request.fetch(
    `${BASE_URL}/api/v1/artists/${id}/images/websearch?type=thumb`,
  );
  if (!searchResp.ok()) {
    throw new Error(`seed: GET images/websearch failed: ${searchResp.status()} ${await searchResp.text()}`);
  }
  const body = await searchResp.json();
  if (body.status !== 'unavailable') {
    throw new Error(
      `seed: web image search for the fixture artist reported status=${JSON.stringify(body.status)}, `
      + 'want "unavailable" -- SW_FORCE_PROVIDER_ERROR=duckduckgo (set by the test-a11y Makefile target '
      + 'and mirrored in .github/workflows/ci.yml) is not reaching the DuckDuckGo adapter, or the '
      + 'provider is not actually enabled',
    );
  }
  if (!Array.isArray(body.unavailable_providers) || !body.unavailable_providers.includes('duckduckgo')) {
    throw new Error(
      `seed: unavailable_providers=${JSON.stringify(body.unavailable_providers)}, want an array containing "duckduckgo"`,
    );
  }
  if (!Array.isArray(body.images) || body.images.length !== 0) {
    throw new Error(`seed: images=${JSON.stringify(body.images)}, want an empty array under injected failure`);
  }
  // Load-bearing (#3229 review, C1/M5): status=unavailable alone does not
  // distinguish the injection seam from a real live outage -- only this
  // header proves errors.Is(err, provider.ErrInjectedFailure) fired.
  if (searchResp.headers()['x-stillwater-websearch-injected-failure'] !== 'true') {
    throw new Error(
      'seed: GET images/websearch reported status=unavailable but did NOT carry '
      + 'X-Stillwater-Websearch-Injected-Failure: true -- the failure may be a REAL live DuckDuckGo '
      + 'outage, not the SW_FORCE_PROVIDER_ERROR seam (the false-pass this assertion exists to catch). '
      + 'Check that SW_FORCE_PROVIDER_ERROR=duckduckgo actually reached this server process.',
    );
  }

  return id;
}
