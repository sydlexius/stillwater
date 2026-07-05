// Regression tests for the Guided Tour button branch fixed in #2228.
//
// Root cause: window.startGuidedTour() (web/static/js/tour.js) decided
// between the full OOBE_CHAIN (dashboard -> artists -> artistDetail, 14
// steps) and the legacy standalone artists-only tour (7 steps) by inspecting
// getCurrentScreen() of the ORIGIN page -- the page the button itself lives
// on. On /guide (and any other page getCurrentScreen() does not recognize),
// that always returned null, so the button silently fell into the standalone
// branch even when the /next lane (and its full chain) was available.
//
// The fix branches on nextLaneEnabled() -- read from the "ux-next-enabled"
// meta tag rendered by layout.templ from AssetPaths.NextLaneEnabled, itself
// derived from the server's SW_UX mode -- which is independent of the
// button's origin page. When the lane is enabled, the button now takes the
// exact same path the post-onboarding auto-tour uses (markTourPending +
// navigate to /next/), so the full chain runs regardless of where the button
// was clicked from.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { JSDOM } from 'jsdom';

const __dirname = dirname(fileURLToPath(import.meta.url));
const TOUR_PATH = resolve(__dirname, '../../web/static/js/tour.js');
const TOUR_SRC = readFileSync(TOUR_PATH, 'utf-8');

function wait(ms) {
    return new Promise((r) => setTimeout(r, ms));
}

// driverStub fakes window.driver.js.driver so tests never touch real
// Driver.js UI. Each driver() call is recorded (config.steps.length is what
// the assertions care about); drive() synchronously simulates the user
// finishing on the last step (hasNextStep() -> false, i.e. "completed"),
// which fires tour.js's onDestroy callback immediately -- this is what lets
// the OOBE chain test below step through all three screens without waiting
// on real popover interaction.
function driverStub(calls) {
    return {
        js: {
            driver: function (config) {
                calls.push(config);
                return {
                    drive: function () {
                        if (typeof config.onDestroyStarted === 'function') {
                            config.onDestroyStarted(null, null, {
                                driver: {
                                    hasNextStep: function () { return false; },
                                    destroy: function () {},
                                },
                            });
                        }
                    },
                };
            },
        },
    };
}

// createPage simulates a single page load of tour.js. Each call is a fresh
// JSDOM instance (there is no real cross-navigation persistence in jsdom), so
// the OOBE-chain test below carries localStorage state across calls itself
// via dumpStorage()/storage.
function createPage({ url, nextEnabled, basePath = '', bodyHtml = '', storage = {}, driverCalls = [] }) {
    const metaNext = nextEnabled === undefined
        ? ''
        : `<meta name="ux-next-enabled" content="${nextEnabled}">`;
    const dom = new JSDOM(
        `<!doctype html><html><head>
            <meta name="htmx-base-path" content="${basePath}">
            ${metaNext}
        </head><body>${bodyHtml}</body></html>`,
        { runScripts: 'dangerously', url },
    );
    const win = dom.window;
    win.driver = driverStub(driverCalls);
    const navigated = [];
    win.swNavigate = (u) => navigated.push(u);
    for (const [k, v] of Object.entries(storage)) {
        win.localStorage.setItem(k, v);
    }
    win.eval(TOUR_SRC);
    return { win, navigated, driverCalls };
}

// dumpStorage snapshots a window's localStorage so the next simulated page
// load can seed from it.
function dumpStorage(win) {
    const out = {};
    for (let i = 0; i < win.localStorage.length; i++) {
        const k = win.localStorage.key(i);
        out[k] = win.localStorage.getItem(k);
    }
    return out;
}

describe('tour.js: startGuidedTour branches on lane availability, not the origin screen (#2228)', () => {
    it('lane enabled: button on /guide enters the OOBE chain via /next/, not /artists', () => {
        const { win, navigated } = createPage({ url: 'http://localhost:1973/guide', nextEnabled: 'true' });
        win.startGuidedTour();
        assert.equal(win.localStorage.getItem('tour.pending'), 'true', 'must mark the tour pending');
        assert.equal(navigated.length, 1);
        assert.equal(navigated[0], '/next/', 'must navigate to the OOBE entry point, not /artists');
    });

    it('lane enabled: button already on the vNext Dashboard still reloads to re-enter the chain runner', () => {
        const { win, navigated } = createPage({ url: 'http://localhost:1973/next/', nextEnabled: 'true' });
        win.startGuidedTour();
        assert.equal(navigated[0], '/next/', 'a standalone start here would skip artists/artistDetail');
    });

    it('lane disabled (SW_UX=stable): button falls back to the legacy /artists standalone tour', () => {
        const { win, navigated } = createPage({ url: 'http://localhost:1973/guide', nextEnabled: 'false' });
        win.startGuidedTour();
        assert.equal(win.localStorage.getItem('tour.pending'), 'true');
        assert.equal(navigated[0], '/artists');
    });

    it('lane disabled: ux-next-enabled meta absent (unpatched/legacy render) still falls back to /artists', () => {
        const { win, navigated } = createPage({ url: 'http://localhost:1973/guide', nextEnabled: undefined });
        win.startGuidedTour();
        assert.equal(navigated[0], '/artists', 'missing meta must default to lane-disabled, never crash or hang');
    });

    it('lane disabled: already on the stable Artists page starts the 7-step standalone tour immediately, no navigation', () => {
        const driverCalls = [];
        const { win, navigated } = createPage({
            url: 'http://localhost:1973/artists',
            nextEnabled: 'false',
            driverCalls,
        });
        win.startGuidedTour();
        assert.equal(navigated.length, 0, 'must not navigate -- already on the target page');
        assert.equal(driverCalls.length, 1);
        assert.equal(driverCalls[0].steps.length, 7, 'standalone artists tour is 7 steps');
    });

    it('base path is honored: lane-enabled navigation is prefixed with htmx-base-path', () => {
        const { win, navigated } = createPage({
            url: 'http://localhost:1973/sw/guide',
            nextEnabled: 'true',
            basePath: '/sw',
        });
        win.startGuidedTour();
        assert.equal(navigated[0], '/sw/next/');
    });
});

describe('tour.js: getCurrentScreen recognizes the promoted canonical dashboard route (#2228 fix-round)', () => {
    it('auto-start on "/" with a pending tour drives the dashboard group, not a silent no-op', async () => {
        const driverCalls = [];
        const errors = [];
        const { win } = createPage({
            url: 'http://localhost:1973/',
            nextEnabled: 'true',
            bodyHtml: `
                <div id="sw-sidebar"></div>
                <div id="dashboard-search"></div>
                <div id="dashboard-run-rules-btn"></div>
                <div id="action-queue"></div>
                <div id="next-dash-activity-feed"></div>
            `,
            storage: { 'tour.pending': 'true' },
            driverCalls,
        });
        win.console.error = (msg) => errors.push(msg);
        await wait(600);
        assert.equal(errors.length, 0, 'must not log "unrecognised screen" for the promoted dashboard route "/"');
        assert.equal(driverCalls.length, 1, 'dashboard tour must actually drive -- a null getCurrentScreen() would silently no-op here');
        assert.equal(driverCalls[0].steps.length, 5, 'full 5-step dashboard group (first chain screen, no nav dedup)');
    });
});

describe('tour.js: button-triggered flow runs the full OOBE chain end-to-end (#2228)', () => {
    it('dashboard -> artists -> artistDetail, ending in tour.completed', async () => {
        const driverCalls = [];

        // Screen 1: /guide button click -- marks pending, navigates to /next/.
        const guide = createPage({ url: 'http://localhost:1973/guide', nextEnabled: 'true' });
        guide.win.startGuidedTour();
        assert.equal(guide.navigated[0], '/next/');
        let storage = dumpStorage(guide.win);
        assert.equal(storage['tour.pending'], 'true');

        // Screen 2: land on the promoted canonical Dashboard route (#1757 -- a
        // real navigation to /next/ is internally re-dispatched server-side
        // and the browser ends up here at "/", not "/next/"; chain index 0,
        // full 5 steps). This is the exact regression the #2228 fix-round
        // caught: getCurrentScreen() previously did not recognize "/" and
        // returned null here, silently aborting the whole chain.
        const dashboard = createPage({
            url: 'http://localhost:1973/',
            nextEnabled: 'true',
            bodyHtml: `
                <div id="sw-sidebar"></div>
                <div id="dashboard-search"></div>
                <div id="dashboard-run-rules-btn"></div>
                <div id="action-queue"></div>
                <div id="next-dash-activity-feed"></div>
            `,
            storage,
            driverCalls,
        });
        await wait(600);
        assert.equal(driverCalls.length, 1, 'dashboard group must auto-run');
        assert.equal(driverCalls[0].steps.length, 5, 'dashboard chain screen keeps its 5 steps (first screen, no dedup)');
        assert.equal(dashboard.navigated[0], '/next/artists', 'must advance to the artists chain screen');
        storage = dumpStorage(dashboard.win);
        assert.equal(storage['tour.chain'], 'artists');

        // Screen 3: land on vNext Artists (chain index 1, nav step de-duped: 7 -> 6).
        const artists = createPage({
            url: 'http://localhost:1973/next/artists',
            nextEnabled: 'true',
            bodyHtml: `
                <div id="sw-sidebar"></div>
                <div id="scan-btn"></div>
                <div id="artist-search"></div>
                <div id="artist-filter-trigger"></div>
                <div id="sort-dropdown"></div>
                <div id="view-toggle"></div>
                <div id="artist-content"><a href="/artists/42">Test Artist</a></div>
            `,
            storage,
            driverCalls,
        });
        await wait(600);
        assert.equal(driverCalls.length, 2, 'artists group must auto-run');
        assert.equal(driverCalls[1].steps.length, 6, 'nav step is de-duped on the second chain screen (7 - 1)');
        assert.equal(artists.navigated[0], '/artists/42', 'must advance to the first artist-detail link picked from the DOM');
        storage = dumpStorage(artists.win);
        assert.equal(storage['tour.chain'], 'artistDetail');
        assert.equal(storage['tour.chain.artistUrl'], '/artists/42');

        // Screen 4: land on artist detail (chain index 2, final screen, 3 steps).
        const detail = createPage({
            url: 'http://localhost:1973/artists/42',
            nextEnabled: 'true',
            bodyHtml: `
                <div id="next-hero-name"></div>
                <div id="next-hero-refresh-btn"></div>
                <div id="next-findings"></div>
            `,
            storage,
            driverCalls,
        });
        await wait(600);
        assert.equal(driverCalls.length, 3, 'artistDetail group must auto-run');
        assert.equal(driverCalls[2].steps.length, 3, 'artistDetail keeps all 3 steps (first step is not #sw-sidebar)');
        assert.equal(detail.navigated.length, 0, 'chain is exhausted -- no further navigation');
        storage = dumpStorage(detail.win);
        assert.equal(storage['tour.completed'], 'true', 'onboarding must be marked complete');
        assert.equal(storage['tour.chain'], undefined, 'chain key must be cleared');
        assert.equal(storage['tour.chain.artistUrl'], undefined, 'chain artist-url key must be cleared');

        const totalStepsDriven = driverCalls.reduce((sum, c) => sum + c.steps.length, 0);
        assert.equal(totalStepsDriven, 14, 'combined chain steps actually driven: 5 + 6 + 3 (nav de-duped once), matching the 14-step OOBE chain');
    });
});

describe('tour.js: shouldAutoStart does not treat null===null as a chain match (fold-in fix)', () => {
    // Root cause: shouldAutoStart() checked `getChainNext() === screen` before
    // checking the pending flag. getChainNext() reads localStorage's
    // 'tour.chain' key, which is null when no chain is active; getCurrentScreen()
    // returns null for any unrecognized path (e.g. /settings). With neither set,
    // `null === null` was true, so shouldAutoStart() fired on ANY unrecognized
    // page -- with no chain in progress and no OOBE pending -- logging
    // "tour: auto-start on unrecognised screen: null" and calling markComplete(),
    // which prematurely flips tour.completed and permanently suppresses the real
    // OOBE auto-start the next time the user actually lands on the dashboard.
    // A broken guard that merely reordered the pending check ahead of the chain
    // check (instead of null-checking screen) would still fail this test the
    // moment a chain key is genuinely absent and pending happens to be unset,
    // so this isolates the null-vs-null identity bug specifically.
    it('unrecognized page, no chain key, no pending flag: no error logged, onboarding not marked complete', async () => {
        const errors = [];
        const { win } = createPage({ url: 'http://localhost:1973/settings', nextEnabled: 'true' });
        win.console.error = (msg) => errors.push(msg);
        await wait(100);
        assert.equal(errors.length, 0, 'must not log "tour: auto-start on unrecognised screen: null"');
        assert.equal(win.localStorage.getItem('tour.completed'), null, 'markComplete() must not fire from a null===null false match');
    });
});
