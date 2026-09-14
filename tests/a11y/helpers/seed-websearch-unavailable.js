// seed-websearch-unavailable.js - fixture for websearch-unavailable.spec.js
// (#3229).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots a brand-new empty database, an empty library, and
// has no reliable outbound network (CLAUDE.md: "MUST build its own fixture
// inside the harness ... make the web search provider fail DETERMINISTICALLY
// without touching the public internet"). A real DuckDuckGo request from
// this harness would either hang or 403 non-deterministically depending on
// the runner's network egress, which is exactly the kind of flake a fixture
// must not depend on.
//
// THE INJECTION SEAM
//
// internal/provider/injection.go's ShouldInjectFailure / SW_FORCE_PROVIDER_ERROR
// is the repo's existing, sanctioned fault-injection seam (already used by
// scripts/smoke-provider-failure.sh, the CI-required "Provider Failure
// Smoke" job). It is wired into duckduckgo.Adapter.SearchImages
// (internal/provider/duckduckgo/duckduckgo.go) exactly like every other
// provider adapter: `if provider.ShouldInjectFailure(a.Name()) { return nil,
// provider.ErrInjectedFailure }`.
//
// The Makefile's test-a11y target sets SW_FORCE_PROVIDER_ERROR=duckduckgo
// for the ephemeral server this harness boots, so every DuckDuckGo web
// search call fails deterministically and offline, with zero code paths
// that could ever fire in a production binary: cmd/stillwater/main.go
// refuses to start (version.IsReleaseBuild() check) if the env var is set
// on a release build, so this seam cannot survive an accidental config copy
// into a real deployment. This fixture does not set the env var itself (it
// cannot -- the server process is already running by the time Playwright's
// globalSetup executes); it only ENABLES the DuckDuckGo web search provider
// (disabled by default) and then PROVES the injected failure actually fires
// by reading back the real API response, so a regression in the injection
// wiring itself fails the fixture loudly rather than the spec silently
// rendering a false pass.
//
// WHAT GETS SEEDED
//
//   1. One artist via a real library scan (there is no artist-create
//      endpoint -- see seed-blast-radius.js for the same constraint).
//   2. The DuckDuckGo web search provider toggled on via
//      PUT /api/v1/providers/websearch/duckduckgo/toggle, so
//      ImageSearchData.WebSearchEnabled is true and the "Web Search" Actions
//      menu entry renders at all.
//   3. A direct GET of the endpoint under test
//      (/api/v1/artists/{id}/images/websearch?type=thumb), asserting
//      status === "unavailable" and unavailable_providers === ["duckduckgo"]
//      BEFORE handing the artist id back -- the fixture's defining property,
//      proven against the real server rather than assumed.

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
      + 'want "unavailable" -- SW_FORCE_PROVIDER_ERROR=duckduckgo (set by the test-a11y Makefile target) '
      + 'is not reaching the DuckDuckGo adapter, or the provider is not actually enabled',
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

  return id;
}
