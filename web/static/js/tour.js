// Guided Tour -- Driver.js step definitions and trigger logic.
// Loaded globally via layout.templ. Implements a per-screen SCREEN_STEPS
// registry (dashboard, artists, artistDetail) and dual launcher APIs:
//   window.startScreenTour()  -- run steps for the current screen only
//   window.startGuidedTour()  -- full OOBE flow (navigates to entry screen first)
//
// Auto-start fires when the OOBE wizard finishes (TOUR_PENDING_KEY set) and
// the user lands on the vNext Dashboard. The OOBE chain runs:
//   dashboard (5 steps) -> artists (6 steps) -> artistDetail (3 steps) = 14 total.
(function() {
    'use strict';

    var TOUR_COMPLETED_KEY = 'tour.completed';
    var TOUR_PENDING_KEY = 'tour.pending';
    // TOUR_CHAIN_KEY stores the next screen name to run during an in-progress
    // OOBE chain. Set after a screen's group completes; cleared when the chain
    // exhausts or the user dismisses mid-way.
    var TOUR_CHAIN_KEY = 'tour.chain';
    // TOUR_CHAIN_ARTIST_URL_KEY stores the artist-detail URL chosen at the end
    // of the artists chain step. Required because the artistDetail URL is
    // dynamic (depends on which artist exists), so it cannot be a static entry
    // in CHAIN_URLS. Cleared alongside TOUR_CHAIN_KEY on completion/dismissal.
    var TOUR_CHAIN_ARTIST_URL_KEY = 'tour.chain.artistUrl';

    // OOBE_CHAIN defines the ordered sequence of screens shown at the end of
    // first-run onboarding. dashboard -> artists -> artistDetail.
    // artistDetail navigation is dynamic: the artists step picks the first
    // artist link from the DOM and stores it in TOUR_CHAIN_ARTIST_URL_KEY.
    var OOBE_CHAIN = ['dashboard', 'artists', 'artistDetail'];

    // CHAIN_URLS maps a chain screen name to the URL to navigate to when
    // advancing the chain. dashboard is never actually read as an advance
    // target (it is always OOBE_CHAIN[0], the entry point, never something the
    // chain advances TO) but is kept here, set to the promoted canonical
    // route (#1757), for documentation consistency with getCurrentScreen().
    // artistDetail is absent: its URL is dynamic and is stored separately in
    // TOUR_CHAIN_ARTIST_URL_KEY.
    var CHAIN_URLS = {
        dashboard: '/',
        artists: '/next/artists'
    };

    // nextChainScreen returns the screen name that follows currentScreen in
    // OOBE_CHAIN, or null when currentScreen is the last entry.
    function nextChainScreen(currentScreen) {
        var idx = OOBE_CHAIN.indexOf(currentScreen);
        if (idx === -1 || idx === OOBE_CHAIN.length - 1) { return null; }
        return OOBE_CHAIN[idx + 1];
    }

    function getChainNext() { return localStorage.getItem(TOUR_CHAIN_KEY); }
    function setChainNext(screen) { localStorage.setItem(TOUR_CHAIN_KEY, screen); }
    function clearChain() { localStorage.removeItem(TOUR_CHAIN_KEY); }

    function getChainArtistUrl() { return localStorage.getItem(TOUR_CHAIN_ARTIST_URL_KEY); }
    function setChainArtistUrl(url) { localStorage.setItem(TOUR_CHAIN_ARTIST_URL_KEY, url); }
    function clearChainArtistUrl() { localStorage.removeItem(TOUR_CHAIN_ARTIST_URL_KEY); }

    // --- Chain-mode helpers (OOBE chained tour only) ---

    // getChainedSteps returns the step list for screenName as it appears in the
    // OOBE chain at chainIdx. The leading Navigation step (#sw-sidebar) is
    // omitted from every chain screen except the first (chainIdx === 0) to
    // eliminate the duplicate the user would otherwise see on screen transitions.
    // IMPORTANT: only the Navigation step is dropped -- a non-Navigation first
    // step (e.g. artistDetail's "Artist Header" at #next-hero-name) is kept.
    function getChainedSteps(screenName, chainIdx) {
        var steps = SCREEN_STEPS[screenName] ? SCREEN_STEPS[screenName]() : [];
        if (chainIdx > 0 && steps.length > 0 && steps[0].element === '#sw-sidebar') {
            steps = steps.slice(1);
        }
        return steps;
    }

    // computeChainTotal returns the total number of steps across all OOBE_CHAIN
    // screens after nav-dedup is applied. Dynamically computed from SCREEN_STEPS
    // so it stays correct if step lists change.
    function computeChainTotal() {
        var total = 0;
        for (var i = 0; i < OOBE_CHAIN.length; i++) {
            total += getChainedSteps(OOBE_CHAIN[i], i).length;
        }
        return total;
    }

    // computeChainOffset returns the cumulative step count for all chain screens
    // before upToIdx, so that step 1 of screen N has global position (offset + 1).
    function computeChainOffset(upToIdx) {
        var offset = 0;
        for (var i = 0; i < upToIdx; i++) {
            offset += getChainedSteps(OOBE_CHAIN[i], i).length;
        }
        return offset;
    }

    // decorateChainProgress clones each step, injecting a per-step
    // popover.progressText with the global chain position ("N of M") so
    // Driver.js displays the cumulative counter rather than the per-instance one.
    function decorateChainProgress(steps, offset, total) {
        var result = [];
        for (var i = 0; i < steps.length; i++) {
            var step = steps[i];
            var k;
            // Shallow-clone the popover so we do not mutate SCREEN_STEPS data.
            var popover = {};
            for (k in step.popover) {
                if (step.popover.hasOwnProperty(k)) { popover[k] = step.popover[k]; }
            }
            popover.progressText = (offset + i + 1) + ' of ' + total;
            // Shallow-clone the step itself.
            var decorated = {};
            for (k in step) {
                if (step.hasOwnProperty(k)) { decorated[k] = step[k]; }
            }
            decorated.popover = popover;
            result.push(decorated);
        }
        return result;
    }

    // Read translated tour strings from the JSON data island rendered by
    // layout.templ. Falls back to an empty object when the element is absent
    // (e.g. on cached pages), so each step provides its own English default.
    var i18n = {};
    try {
        var el = document.getElementById('tour-i18n');
        if (el && el.dataset.i18n) { i18n = JSON.parse(el.dataset.i18n); }
    } catch (e) { console.warn('tour: failed to parse i18n data', e); }

    // Resolve the base path from the meta tag set by layout.templ.
    function basePath() {
        var el = document.querySelector('meta[name="htmx-base-path"]');
        return el ? el.content : '';
    }

    // nextLaneEnabled reads the ux-next-enabled meta tag set by layout.templ
    // from AssetPaths.NextLaneEnabled (#2228). True whenever the server's
    // SW_UX mode is "next" or "dual" -- i.e. the /next preview lane (and its
    // OOBE_CHAIN) is reachable at all -- REGARDLESS of which channel the
    // page that loaded tour.js happens to be rendering as. This is the
    // channel-availability check startGuidedTour() branches on instead of
    // getCurrentScreen() of the origin page, which returns null (and so
    // always looked like "stable") on origin pages such as /guide that
    // getCurrentScreen() does not recognize.
    function nextLaneEnabled() {
        var el = document.querySelector('meta[name="ux-next-enabled"]');
        return !!el && el.content === 'true';
    }

    // navigate: assign to window.location.href. Callers pass a fully-prefixed
    // URL (basePath already applied); unlike keyboard.js's navigate, this does
    // not add the base path itself. Tests may override via window.swNavigate
    // (mirrors the seam in keyboard.js).
    function navigate(url) {
        if (typeof window.swNavigate === 'function') {
            window.swNavigate(url);
        } else {
            window.location.href = url;
        }
    }

    // getCurrentScreen inspects window.location.pathname and returns a screen
    // identifier for the SCREEN_STEPS registry, or null for unrecognized paths.
    //   'dashboard'   -- / (promoted canonical dashboard, #1757), or the
    //                    legacy /next, /next/ (kept for dual-mode direct visits
    //                    and the nextFallback re-dispatch, which serves the
    //                    same dashboard content without changing the URL)
    //   'artists'     -- /next/artists or /artists (both channels)
    //   'artistDetail'-- /artists/{id} (or legacy /next/artists/{id})
    //   null          -- any other path
    function getCurrentScreen() {
        var bp = basePath();
        var path = window.location.pathname;
        // Strip deployment base-path prefix so comparisons work in sub-path deploys.
        if (bp && path.indexOf(bp) === 0) {
            path = path.slice(bp.length) || '/';
        }
        // '/' is the promoted canonical dashboard route (#1757 PR-6b landed the
        // last promotion; handleIndex serves the same IndexPage regardless of
        // SW_UX channel). This must be recognized in addition to /next, /next/
        // -- without it, navigating the OOBE chain's entry point (which lands
        // here) returned null and silently aborted the tour (#2228 fix-round).
        if (path === '/') { return 'dashboard'; }
        if (path === '/next' || path === '/next/') { return 'dashboard'; }
        // Artist detail must be checked before the artists list pattern.
        // Both the canonical /artists/{id} (promoted in #1757 PR-3b) and the
        // legacy /next/artists/{id} (still reachable via the /next/* fallback
        // re-dispatch) count, but the list pages and the /artists/{id}/images
        // page must not.
        if (/^\/(?:next\/)?artists\/[^/]+$/.test(path)) { return 'artistDetail'; }
        if (path === '/next/artists' || path === '/next/artists/') { return 'artists'; }
        if (path === '/artists' || path === '/artists/') { return 'artists'; }
        return null;
    }

    // isArtistsPage returns true when the current path is the stable or vNext
    // artists list page. Kept for internal use and backward compatibility.
    function isArtistsPage() {
        return getCurrentScreen() === 'artists';
    }

    function shouldAutoStart() {
        // Never fire once onboarding is marked done.
        if (localStorage.getItem(TOUR_COMPLETED_KEY)) { return false; }
        var screen = getCurrentScreen();
        // Chain-in-progress: a prior chain step navigated here. The pending
        // key may already be gone (cleared by markComplete); rely on the chain
        // key alone as the authority for subsequent chain screens.
        // Guard against null === null: an unrecognized screen must never
        // count as a chain match just because no chain is active either.
        if (screen !== null && getChainNext() === screen) { return true; }
        // OOBE initial trigger: pending marker set, on a chain-eligible screen.
        if (localStorage.getItem(TOUR_PENDING_KEY) !== 'true') { return false; }
        // Auto-start on the vNext Dashboard (new OOBE entry point) or the
        // stable Artists page (legacy entry point for the guided tour).
        return screen === 'dashboard' || screen === 'artists';
    }

    function markComplete() {
        localStorage.removeItem(TOUR_PENDING_KEY);
        localStorage.setItem(TOUR_COMPLETED_KEY, 'true');
    }

    // pickFirstArtistUrl scans the current page for the first artist-detail
    // link (href matching /artists/<single-segment>, the canonical detail URL
    // since #1757 PR-3b; a legacy /next/artists/<id> href also matches) and
    // returns the href, or null when no artists are present (empty library
    // guard).
    function pickFirstArtistUrl() {
        var links = document.querySelectorAll('a[href]');
        for (var i = 0; i < links.length; i++) {
            var href = links[i].getAttribute('href');
            if (/\/artists\/[^/]+$/.test(href)) {
                return href;
            }
        }
        return null;
    }

    // SCREEN_STEPS is a registry of per-screen step group factories.
    // Each key maps to a function returning the step array for that screen.
    // Using functions allows lazy i18n lookup -- i18n is populated at module
    // init time so the values are available when these are called.
    var SCREEN_STEPS = {
        // Dashboard steps: target vNext Dashboard element IDs.
        dashboard: function() {
            return [
                {
                    element: '#sw-sidebar',
                    popover: {
                        title: i18n.nav_title || 'Navigation',
                        description: i18n.nav_desc || 'Use the sidebar to switch between Dashboard, Artists, Reports, and Settings. Collapse it with the arrow at the top for more space.',
                        side: 'right',
                        align: 'start'
                    }
                },
                {
                    element: '#dashboard-search',
                    popover: {
                        title: i18n.dash_search_title || 'Search the Queue',
                        description: i18n.dash_search_desc || 'Filter the action queue by artist name to quickly find items that need attention.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#dashboard-run-rules-btn',
                    popover: {
                        title: i18n.dash_run_rules_title || 'Run All Rules',
                        description: i18n.dash_run_rules_desc || 'Evaluate every rule against every artist in one pass. Issues that can be auto-fixed are resolved immediately; others are queued below for review.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#action-queue',
                    popover: {
                        title: i18n.dash_queue_title || 'Action Queue',
                        description: i18n.dash_queue_desc || 'Artists with rule violations or metadata gaps appear here. Click an item to jump to that artist and fix the issue.',
                        side: 'top'
                    }
                },
                {
                    element: '#next-dash-activity-feed',
                    popover: {
                        title: i18n.dash_activity_title || 'Activity Feed',
                        description: i18n.dash_activity_desc || 'Recent Stillwater activity -- scans, metadata fetches, and publishes -- streams here in real time.',
                        side: 'left',
                        align: 'start'
                    }
                }
            ];
        },

        // Artists steps: target IDs shared between stable and vNext artists pages.
        artists: function() {
            return [
                {
                    element: '#sw-sidebar',
                    popover: {
                        title: i18n.nav_title || 'Navigation',
                        description: i18n.nav_desc || 'Use the sidebar to switch between Dashboard, Artists, Reports, and Settings. Collapse it with the arrow at the top for more space.',
                        side: 'right',
                        align: 'start'
                    }
                },
                {
                    element: '#scan-btn',
                    popover: {
                        title: i18n.scan_title || 'Scan Your Library',
                        description: i18n.scan_desc || 'Click here to scan your music folders. Stillwater will discover artists and fetch their metadata automatically.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-search',
                    popover: {
                        title: i18n.search_title || 'Search Artists',
                        description: i18n.search_desc || 'Type a name to instantly filter your artist list.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-filter-trigger',
                    popover: {
                        title: i18n.filter_title || 'Filter Artists',
                        description: i18n.filter_desc || 'Narrow results by metadata status, image presence, artist type, or library.',
                        side: 'bottom'
                    }
                },
                {
                    // #sort-dropdown carries the "hidden" class (display:none)
                    // whenever the page loads in TABLE view -- the default view
                    // -- because table view sorts via its clickable column
                    // headers instead (see artists.templ, M55 #1335). Driver.js
                    // cannot usefully highlight a display:none element (its
                    // bounding rect collapses to 0x0, so the popover anchors
                    // nowhere); the onHighlightStarted/onDeselected pair below
                    // temporarily strips the class for the duration of this
                    // step only, then restores it exactly as found so table
                    // view's real state is untouched once the tour moves on.
                    element: '#sort-dropdown',
                    popover: {
                        title: i18n.sort_title || 'Sort Artists',
                        description: i18n.sort_desc || 'Reorder by name, health score, date added, or last updated.',
                        side: 'bottom'
                    },
                    onHighlightStarted: function(element) {
                        if (element && element.classList.contains('hidden')) {
                            element.classList.remove('hidden');
                            element.setAttribute('data-tour-forced-visible', 'true');
                        }
                    },
                    onDeselected: function(element) {
                        if (element && element.getAttribute('data-tour-forced-visible') === 'true') {
                            element.classList.add('hidden');
                            element.removeAttribute('data-tour-forced-visible');
                        }
                    }
                },
                {
                    element: '#view-toggle',
                    popover: {
                        title: i18n.view_title || 'Switch Views',
                        description: i18n.view_desc || 'Toggle between a detailed table view and a visual grid view.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-content',
                    popover: {
                        title: i18n.artist_list_title || 'Your Artist List',
                        description: i18n.artist_list_desc || 'Your artists appear here after scanning. Click any artist to view and edit their metadata, images, and platform connections.',
                        side: 'top'
                    }
                }
            ];
        },

        // Artist detail steps: target vNext artist-detail element IDs.
        // Highlights non-obvious features. Used both in the OOBE chain (last
        // screen, navigated to via the first artist link) and via startScreenTour().
        artistDetail: function() {
            return [
                {
                    element: '#next-hero-name',
                    popover: {
                        title: i18n.detail_hero_title || 'Artist Header',
                        description: i18n.detail_hero_desc || 'The artist name, type, and primary artwork are shown here. Click the artwork to open the image manager.',
                        side: 'bottom'
                    }
                },
                {
                    // #next-hero-refresh-btn is the stable ID on the visible
                    // Refresh button in the hero toolbar. The old #refresh-panel
                    // target starts empty (height 0) so the popover had no
                    // anchor; the button itself is always visible with real height.
                    element: '#next-hero-refresh-btn',
                    popover: {
                        title: i18n.detail_refresh_title || 'Refresh Metadata',
                        description: i18n.detail_refresh_desc || 'Pull fresh metadata from your configured providers. You can choose which fields to update before confirming.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#next-findings',
                    popover: {
                        title: i18n.detail_findings_title || 'Rule Findings',
                        description: i18n.detail_findings_desc || 'Rule violations specific to this artist appear here. Fix or dismiss each finding individually, or run all rules at once.',
                        side: 'top'
                    }
                }
            ];
        }
    };

    // createTour builds a Driver.js tour instance with the given steps.
    // The optional onDestroy(completed) callback is invoked when the tour is
    // closed or completed (before Driver.js tears down its overlay).
    // completed is true when the user was on the final step at destroy time
    // (clicked Done or X on the last step); false when dismissed early.
    //
    // The optional chainOpts object activates chain-mode overrides:
    //   chainOpts.isLastScreen {boolean} -- when false, the Done button is
    //   relabeled "Next ->" so the user understands the button advances to the
    //   next chain screen rather than finishing. Steps should already carry
    //   per-step popover.progressText from decorateChainProgress().
    function createTour(steps, onDestroy, chainOpts) {
        var driverConstructor = window.driver.js.driver;
        var config = {
            popoverClass: 'sw-tour-popover',
            showProgress: true,
            progressText: '{{current}} / {{total}}',
            allowClose: true,
            overlayOpacity: 0.5,
            stagePadding: 8,
            stageRadius: 8,
            animate: true,
            smoothScroll: true,
            steps: steps,
            onDestroyStarted: function(_, __, opts) {
                // Called when the user clicks X, the overlay, or Done on the
                // last step. Driver.js does not bind `this`, so use opts.driver.
                // hasNextStep() is false only when the active step is the last one
                // (user finished or closed on the final step -- treat as complete).
                var completed = !!(opts && opts.driver && !opts.driver.hasNextStep());
                if (typeof onDestroy === 'function') { onDestroy(completed); }
                if (opts && opts.driver && typeof opts.driver.destroy === 'function') {
                    opts.driver.destroy();
                }
            }
        };
        if (chainOpts) {
            // Chain-scoped counter: per-step popover.progressText carries the
            // "N of M" string; set a matching format string so the fallback is
            // coherent if a step lacks its override.
            config.progressText = '{{current}} of {{total}}';
            // On non-final chain screens, the Done button advances to the next
            // screen -- relabel it so the intent is visible to the user.
            if (!chainOpts.isLastScreen) {
                config.doneBtnText = 'Next →';
            }
        }
        return driverConstructor(config);
    }

    // waitForTourTargets waits for the given CSS selector to appear in the DOM
    // before resolving. Uses a MutationObserver with a fallback timeout so the
    // tour never hangs indefinitely when a target is absent.
    function waitForTourTargets(selector, timeoutMs) {
        return new Promise(function(resolve) {
            if (document.querySelector(selector)) {
                resolve();
                return;
            }
            var resolved = false;
            var observer = new MutationObserver(function() {
                if (!resolved && document.querySelector(selector)) {
                    resolved = true;
                    observer.disconnect();
                    resolve();
                }
            });
            observer.observe(document.body, { childList: true, subtree: true });
            setTimeout(function() {
                if (!resolved) {
                    resolved = true;
                    observer.disconnect();
                    resolve();
                }
            }, timeoutMs || 3000);
        });
    }

    // Mark the tour as pending so it auto-starts on the next page load.
    // Centralizes localStorage key management so other templates (e.g. onboarding)
    // do not need to reference key names directly.
    window.markTourPending = function() {
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        localStorage.setItem(TOUR_PENDING_KEY, 'true');
    };

    // startScreenTour runs the tour for the current page only. Use this for
    // "take a tour" buttons on individual screens. Logs an error when no
    // SCREEN_STEPS group is registered for the current path (fail loudly).
    window.startScreenTour = function() {
        var screen = getCurrentScreen();
        if (!screen || !SCREEN_STEPS[screen]) {
            console.error('tour: no tour defined for screen: ' + (screen || window.location.pathname));
            return;
        }
        var steps = SCREEN_STEPS[screen]();
        if (!steps || steps.length === 0) {
            console.error('tour: empty step list for screen: ' + screen);
            return;
        }
        var firstSelector = steps[0].element;
        waitForTourTargets(firstSelector, 3000).then(function() {
            var tour = createTour(steps, markComplete);
            tour.drive();
        });
    };

    // startGuidedTour is the public API for the full OOBE / manual restart
    // flow, wired to the Guided Tour button (e.g. on /guide, #2228). It must
    // decide between two very different experiences:
    //   - full OOBE_CHAIN (14 steps: dashboard -> artists -> artistDetail)
    //   - legacy standalone artists-only tour (7 steps)
    //
    // That decision used to branch on getCurrentScreen() of the ORIGIN page
    // (the page the button lives on). On /guide -- and any other page
    // getCurrentScreen() does not recognize -- that always returned null,
    // which fell through to the stable/standalone branch even when the /next
    // lane (and its full chain) was available. The origin page tells you
    // nothing about lane availability, so branch on nextLaneEnabled()
    // instead: whether the /next lane is reachable AT ALL under the server's
    // SW_UX mode, independent of the page the button was clicked from.
    //
    // When the lane is enabled, route through the exact same path the
    // post-onboarding auto-tour uses (markTourPending + navigate to the /next
    // entry point) so shouldAutoStart() drives the full chain on arrival --
    // regardless of whether the button's origin page happens to already be
    // the vNext Dashboard (a reload is required there too, so
    // shouldAutoStart() re-enters the chain runner rather than a one-off
    // standalone start that would skip artists/artistDetail).
    window.startGuidedTour = function() {
        var bp = basePath();

        if (nextLaneEnabled()) {
            window.markTourPending();
            navigate(bp + '/next/');
            return;
        }

        // Lane unavailable (SW_UX=stable): fall back to the legacy standalone
        // artists-only tour, navigating to /artists if not already there.
        if (!isArtistsPage()) {
            window.markTourPending();
            navigate(bp + '/artists');
            return;
        }
        // Already on the stable Artists page -- start immediately.
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        var artistSteps = SCREEN_STEPS.artists();
        var artistTour = createTour(artistSteps, markComplete);
        artistTour.drive();
    };

    // Auto-start: triggered by the OOBE wizard redirect or an in-progress chain
    // navigation. Runs the current screen's step group, then either advances
    // to the next chain screen or marks onboarding complete.
    //
    // Chain order: dashboard -> artists -> artistDetail (14 total steps after
    // nav-dedup). artistDetail navigation is dynamic: the artists step picks
    // the first artist link from the DOM and stores it in localStorage.
    //
    // Completion vs. dismissal: onDestroy receives a boolean that is true when
    // the driver was on its final step at teardown. Dismissed mid-chain ->
    // end onboarding immediately (markComplete + clearChain, no navigation).
    if (shouldAutoStart()) {
        var autoScreen = getCurrentScreen();
        if (!SCREEN_STEPS[autoScreen]) {
            // Unrecognised screen -- end onboarding gracefully rather than hanging.
            console.error('tour: auto-start on unrecognised screen: ' + autoScreen);
            markComplete();
            clearChain();
        } else {
            // Determine whether this auto-start is part of the OOBE chain.
            // The chain begins at OOBE_CHAIN[0] (dashboard) on first-run and
            // advances via TOUR_CHAIN_KEY. A standalone artists auto-start on
            // the stable channel (TOUR_PENDING_KEY set, no chain key) is NOT a
            // chain run even though 'artists' appears in OOBE_CHAIN.
            var autoChainIdx = OOBE_CHAIN.indexOf(autoScreen);
            var isChainRun = autoChainIdx !== -1 &&
                (autoChainIdx === 0 || getChainNext() === autoScreen);

            // In chain mode: drop the duplicate Navigation step from every
            // screen except the first so the user sees it only once (fix #3).
            // In standalone mode: use the full step list as-is.
            var autoSteps = isChainRun
                ? getChainedSteps(autoScreen, autoChainIdx)
                : SCREEN_STEPS[autoScreen]();

            var autoFirstSelector = autoSteps[0].element;
            waitForTourTargets(autoFirstSelector, 3000).then(function() {
                setTimeout(function() {
                    // Guard: if the first target never appeared (timeout elapsed),
                    // skip this chain entry rather than driving a broken tour.
                    if (!document.querySelector(autoFirstSelector)) {
                        console.warn('tour: first target absent after wait, skipping screen: ' + autoScreen);
                        var skipNext = nextChainScreen(autoScreen);
                        if (skipNext) {
                            if (skipNext === 'artistDetail') {
                                var skipArtistUrl = pickFirstArtistUrl();
                                if (!skipArtistUrl) {
                                    // No artists -- end chain gracefully.
                                    markComplete();
                                    clearChain();
                                    clearChainArtistUrl();
                                    return;
                                }
                                setChainArtistUrl(skipArtistUrl);
                                setChainNext(skipNext);
                                navigate(skipArtistUrl);
                            } else {
                                setChainNext(skipNext);
                                navigate(basePath() + CHAIN_URLS[skipNext]);
                            }
                        } else {
                            markComplete();
                            clearChain();
                            clearChainArtistUrl();
                        }
                        return;
                    }

                    // Chain-mode: decorate each step with the cumulative "N of M"
                    // progress label (fix #1) and configure the Done-button label
                    // (fix #2). Standalone mode: pass steps unmodified.
                    var stepsToRun = autoSteps;
                    var chainOpts = null;
                    if (isChainRun) {
                        var chainTotal = computeChainTotal();
                        var chainOffset = computeChainOffset(autoChainIdx);
                        var isLastChainScreen = nextChainScreen(autoScreen) === null;
                        stepsToRun = decorateChainProgress(autoSteps, chainOffset, chainTotal);
                        chainOpts = { isLastScreen: isLastChainScreen };
                    }

                    var autoTour = createTour(stepsToRun, function(completed) {
                        if (!completed) {
                            // User dismissed mid-chain -- end onboarding immediately.
                            markComplete();
                            clearChain();
                            clearChainArtistUrl();
                            return;
                        }
                        if (!isChainRun) {
                            // Standalone pending run -- don't leak into the OOBE chain.
                            markComplete();
                            clearChain();
                            clearChainArtistUrl();
                            return;
                        }
                        // Group completed. Advance to the next chain screen or finish.
                        var chainNext = nextChainScreen(autoScreen);
                        if (chainNext) {
                            if (chainNext === 'artistDetail') {
                                // artistDetail URL is dynamic: pick the first artist
                                // link from the current page (artists list).
                                var artistUrl = pickFirstArtistUrl();
                                if (!artistUrl) {
                                    // Empty library -- finish the chain gracefully
                                    // rather than navigating to a broken URL.
                                    markComplete();
                                    clearChain();
                                    clearChainArtistUrl();
                                    return;
                                }
                                setChainArtistUrl(artistUrl);
                                setChainNext(chainNext);
                                navigate(artistUrl);
                            } else {
                                setChainNext(chainNext);
                                navigate(basePath() + CHAIN_URLS[chainNext]);
                            }
                        } else {
                            // Chain exhausted -- onboarding complete.
                            markComplete();
                            clearChain();
                            clearChainArtistUrl();
                        }
                    }, chainOpts);
                    autoTour.drive();
                }, 500);
            });
        }
    }
})();
