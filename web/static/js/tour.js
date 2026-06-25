// Guided Tour -- Driver.js step definitions and trigger logic.
// Loaded globally via layout.templ. Implements a per-screen SCREEN_STEPS
// registry (dashboard, artists, artistDetail) and dual launcher APIs:
//   window.startScreenTour()  -- run steps for the current screen only
//   window.startGuidedTour()  -- full OOBE flow (navigates to entry screen first)
//
// Auto-start fires when the OOBE wizard finishes (TOUR_PENDING_KEY set) and
// the user lands on the vNext Dashboard or the stable Artists page.
(function() {
    'use strict';

    var TOUR_COMPLETED_KEY = 'tour.completed';
    var TOUR_PENDING_KEY = 'tour.pending';

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

    // getCurrentScreen inspects window.location.pathname and returns a screen
    // identifier for the SCREEN_STEPS registry, or null for unrecognized paths.
    //   'dashboard'   -- /next or /next/
    //   'artists'     -- /next/artists or /artists (both channels)
    //   'artistDetail'-- /next/artists/{id}
    //   null          -- any other path
    function getCurrentScreen() {
        var bp = basePath();
        var path = window.location.pathname;
        // Strip deployment base-path prefix so comparisons work in sub-path deploys.
        if (bp && path.indexOf(bp) === 0) {
            path = path.slice(bp.length) || '/';
        }
        if (path === '/next' || path === '/next/') { return 'dashboard'; }
        // Artist detail must be checked before the artists list pattern.
        if (/^\/next\/artists\/[^/]+/.test(path)) { return 'artistDetail'; }
        if (path === '/next/artists' || path === '/next/artists/') { return 'artists'; }
        if (path === '/artists' || path === '/artists/') { return 'artists'; }
        return null;
    }

    // isArtistsPage returns true when the current path is the stable or vNext
    // artists list page. Kept for internal use and backward compatibility.
    function isArtistsPage() {
        return getCurrentScreen() === 'artists';
    }

    function isDashboardPage() {
        return getCurrentScreen() === 'dashboard';
    }

    function shouldAutoStart() {
        if (localStorage.getItem(TOUR_PENDING_KEY) !== 'true') { return false; }
        if (localStorage.getItem(TOUR_COMPLETED_KEY)) { return false; }
        var screen = getCurrentScreen();
        // Auto-start on the vNext Dashboard (new OOBE entry point) or the
        // stable Artists page (legacy entry point for the guided tour).
        return screen === 'dashboard' || screen === 'artists';
    }

    function markComplete() {
        localStorage.removeItem(TOUR_PENDING_KEY);
        localStorage.setItem(TOUR_COMPLETED_KEY, 'true');
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
                    element: '#sort-dropdown',
                    popover: {
                        title: i18n.sort_title || 'Sort Artists',
                        description: i18n.sort_desc || 'Reorder by name, health score, date added, or last updated.',
                        side: 'bottom'
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
        // Highlights non-obvious features; available via startScreenTour() only
        // (the OOBE flow does not auto-navigate to a specific artist).
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
                    element: '#refresh-panel',
                    popover: {
                        title: i18n.detail_refresh_title || 'Refresh Metadata',
                        description: i18n.detail_refresh_desc || 'Pull fresh metadata from your configured providers. You can choose which fields to update before confirming.',
                        side: 'top'
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
    // The optional onDestroy callback is invoked when the tour is closed or
    // completed (before Driver.js tears down its overlay).
    function createTour(steps, onDestroy) {
        var driverConstructor = window.driver.js.driver;
        return driverConstructor({
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
                if (typeof onDestroy === 'function') { onDestroy(); }
                if (opts && opts.driver && typeof opts.driver.destroy === 'function') {
                    opts.driver.destroy();
                }
            }
        });
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
    // flow. On vNext pages it navigates to the Dashboard first; on the stable
    // channel it navigates to /artists as before.
    window.startGuidedTour = function() {
        var bp = basePath();
        var screen = getCurrentScreen();

        // vNext path: navigate to Dashboard if not already there.
        if (screen !== null && window.location.pathname.indexOf(bp + '/next') === 0) {
            if (!isDashboardPage()) {
                window.markTourPending();
                window.location.href = bp + '/next/';
                return;
            }
            // Already on the Dashboard -- start immediately.
            localStorage.removeItem(TOUR_COMPLETED_KEY);
            var dashSteps = SCREEN_STEPS.dashboard();
            var dashTour = createTour(dashSteps, markComplete);
            dashTour.drive();
            return;
        }

        // Stable channel path: navigate to /artists if not already there.
        if (!isArtistsPage()) {
            window.markTourPending();
            window.location.href = bp + '/artists';
            return;
        }
        // Already on the stable Artists page -- start immediately.
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        var artistSteps = SCREEN_STEPS.artists();
        var artistTour = createTour(artistSteps, markComplete);
        artistTour.drive();
    };

    // Auto-start: triggered by the OOBE wizard redirect. Waits for the first
    // step's target element to appear (HTMX may still be hydrating), then
    // allows a brief grace period before driving.
    if (shouldAutoStart()) {
        var screen = getCurrentScreen();
        var autoSteps = (screen === 'dashboard')
            ? SCREEN_STEPS.dashboard()
            : SCREEN_STEPS.artists();
        var firstSelector = autoSteps[0].element;
        waitForTourTargets(firstSelector, 3000).then(function() {
            setTimeout(function() {
                var tour = createTour(autoSteps, markComplete);
                tour.drive();
            }, 500);
        });
    }
})();
