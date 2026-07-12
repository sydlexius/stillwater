// auth.js - thin adapter over the a11y tier's existing login helper so the
// responsive harness does not duplicate auth/CSRF logic (single source of
// truth: tests/a11y/helpers/bootstrap.js), plus the target-server context the
// report needs in order to be interpretable at all.
//
// !! THIS HARNESS MUTATES THE TARGET SERVER. !!
//
// bootstrap.js POSTs /api/v1/auth/setup (which CREATES an admin account on an
// instance that has not completed setup) and PUTs /api/v1/settings. On top of
// that, forcing each theme goes through the app's REAL preference-set path, so
// a full matrix run issues ~130 PUT /api/v1/preferences/theme writes and would
// leave the account on whichever theme happened to run last. Point this at a
// throwaway UAT instance, never at a real one. run.js requires --url explicitly
// (no default) for exactly this reason, and brackets the run with
// readThemePreference/writeThemePreference below so the account's original
// theme is restored.
//
// bootstrap.js reads its target server from SW_TEST_URL/SW_PORT at MODULE LOAD
// time, so this uses a dynamic import: env vars must be set before the first
// call to authenticateOnce() in the process (run.js does this before calling
// in).
//
// IMPORTANT: /api/v1/auth/login sits behind the production login brute-force
// rate limiter (5 req/min/IP, shared across all auth endpoints -- see
// tests/a11y/global-setup.js). Logging in once per browser context (one per
// viewport x theme combo) blows through that budget in a couple of iterations
// and 429s the rest of the run. Authenticate EXACTLY ONCE per process via
// authenticateOnce(), capture the resulting storageState, and hand that state
// to every browser.newContext({ storageState }) call instead of logging in
// again.

// authenticateOnce requires BOTH credentials explicitly.
//
// bootstrap.js falls back to `ci-a11y-admin` / a password COMMITTED TO GIT when
// its env vars are unset, and it calls POST /api/v1/auth/setup -- so against an
// instance that has not completed setup, an unset-credentials run would
// PROVISION AN ADMIN ACCOUNT WITH A PUBLIC PASSWORD. That fallback is
// defensible inside the a11y tier (ephemeral CI database, torn down with the
// job); this harness is pointed at whatever instance an operator names, which
// makes it indefensible here. Fail rather than fall through.
export async function authenticateOnce({ baseURL, adminUser, adminPass } = {}) {
  if (!baseURL) throw new Error('authenticateOnce: baseURL is required');
  if (!adminUser || !adminPass) {
    throw new Error(
      'STILLWATER_ADMIN_USER and STILLWATER_ADMIN_PASSWORD must both be set. This harness '
      + 'will NOT fall back to the a11y tier\'s committed CI defaults '
      + '(tests/a11y/helpers/bootstrap.js): against an instance that has not completed '
      + 'setup, POST /api/v1/auth/setup would provision an admin account with a password '
      + 'that is public in git history.'
    );
  }
  process.env.SW_TEST_URL = baseURL;
  process.env.STILLWATER_ADMIN_USER = adminUser;
  process.env.STILLWATER_ADMIN_PASSWORD = adminPass;

  const { setupAndLogin } = await import('../../a11y/helpers/bootstrap.js');
  const { request } = await import('playwright');

  const ctx = await request.newContext({ baseURL });
  try {
    await setupAndLogin(ctx);
    return ctx.storageState();
  } finally {
    await ctx.dispose();
  }
}

async function withApi({ baseURL, storageState }, fn) {
  const { request } = await import('playwright');
  const ctx = await request.newContext({ baseURL, storageState });
  try {
    return await fn(ctx);
  } finally {
    await ctx.dispose();
  }
}

// State-changing API calls need the CSRF token as a HEADER; the cookie alone is
// not enough (double-submit). storageState carries the cookie, so pull it back
// out of there rather than threading a second value through every call site.
function csrfFrom(storageState) {
  const cookie = (storageState.cookies || []).find(c => c.name === 'csrf_token');
  return cookie ? cookie.value : '';
}

// resolveFirstArtistId looks up a real artist id from the target database via
// GET /api/v1/artists, so the artist-detail probe (run.js) never hardcodes an
// id that would only exist in one particular database snapshot.
//
// Returns { id, ... } on success or { id: null, reason } on failure -- and the
// reason DISTINGUISHES the failure modes. The pre-fix version was
// `catch { return null }`, which collapsed "auth broke", "the API changed
// shape" and "this database has no artists" into one indistinguishable null;
// run.js then dropped the page from the report entirely with a console warning,
// so a consumer diffing baselines saw 10 pages where the previous run had 11
// and could not tell a skip from a clean pass.
export async function resolveFirstArtistId({ baseURL, storageState } = {}) {
  return withApi({ baseURL, storageState }, async ctx => {
    let res;
    try {
      res = await ctx.get('/api/v1/artists?page=1&page_size=1');
    } catch (err) {
      return { id: null, reason: `GET /api/v1/artists threw: ${err.message}` };
    }
    if (!res.ok()) {
      return { id: null, reason: `GET /api/v1/artists returned HTTP ${res.status()}` };
    }
    let body;
    try {
      body = await res.json();
    } catch (err) {
      return { id: null, reason: `GET /api/v1/artists returned non-JSON: ${err.message}` };
    }
    if (!Array.isArray(body.artists)) {
      return {
        id: null,
        reason: 'GET /api/v1/artists response has no `artists` array -- the API shape changed',
      };
    }
    if (body.artists.length === 0) {
      return { id: null, reason: 'the target database contains no artists' };
    }
    if (!body.artists[0].id) {
      return { id: null, reason: 'the first artist record has no `id` field' };
    }
    return { id: body.artists[0].id };
  });
}

// readThemePreference / writeThemePreference bracket the run so it can put the
// account's theme back the way it found it (see the mutation warning at the top
// of this file). Both return null / false rather than throwing: failing to
// restore a preference must not sink a completed measurement run, but run.js
// reports the failure rather than swallowing it.
export async function readThemePreference({ baseURL, storageState } = {}) {
  return withApi({ baseURL, storageState }, async ctx => {
    const res = await ctx.get('/api/v1/preferences');
    if (!res.ok()) return null;
    const body = await res.json();
    return body.theme ?? null;
  });
}

export async function writeThemePreference({ baseURL, storageState, theme } = {}) {
  return withApi({ baseURL, storageState }, async ctx => {
    const res = await ctx.put('/api/v1/preferences/theme', {
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfFrom(storageState),
      },
      data: JSON.stringify({ value: theme }),
    });
    return res.ok();
  });
}

// describeTarget captures the DATABASE-DEPENDENT context that a baseline number
// is only meaningful against. Settings' offender counts scale with the number of
// libraries and connections configured (each renders its own row of controls),
// and artist-detail resolves to "whatever artist this database returns first" --
// so "N sub-44px targets on settings" is not reproducible on a bare CI runner,
// or on another machine, without knowing these. Recording them lets a baseline
// diff distinguish a real regression from a different database.
//
// A fixed seed database would make these baselines genuinely portable; that is
// a legitimate follow-up, not something this harness should build inline.
export async function describeTarget({ baseURL, storageState } = {}) {
  return withApi({ baseURL, storageState }, async ctx => {
    const fetchAndPick = async (path, pick) => {
      try {
        const res = await ctx.get(path);
        if (!res.ok()) return `unavailable (HTTP ${res.status()})`;
        return pick(await res.json());
      } catch (err) {
        return `unavailable (${err.message})`;
      }
    };
    const len = b => (Array.isArray(b) ? b.length : 'unavailable (unexpected shape)');
    const [health, libraryCount, connectionCount, artistCount] = await Promise.all([
      fetchAndPick('/api/v1/health', b => ({ version: b.version, commit: b.commit })),
      fetchAndPick('/api/v1/libraries', len),
      fetchAndPick('/api/v1/connections', len),
      fetchAndPick('/api/v1/artists?page=1&page_size=1', b => b.total ?? len(b.artists)),
    ]);
    return {
      serverVersion: health && health.version,
      serverCommit: health && health.commit,
      libraryCount,
      connectionCount,
      artistCount,
    };
  });
}
