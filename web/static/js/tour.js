// Guided Tour -- Driver.js step definitions and trigger logic.
// Loaded globally via layout.templ. Only executes on the Artists page
// (auto-start) or when manually invoked via startGuidedTour().
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

    function isArtistsPage() {
        var bp = basePath();
        var path = window.location.pathname;
        return path === bp + '/artists' || path === bp + '/artists/';
    }

    function shouldAutoStart() {
        return localStorage.getItem(TOUR_PENDING_KEY) === 'true' &&
               !localStorage.getItem(TOUR_COMPLETED_KEY) &&
               isArtistsPage();
    }

    function markComplete() {
        localStorage.removeItem(TOUR_PENDING_KEY);
        localStorage.setItem(TOUR_COMPLETED_KEY, 'true');
    }

    function createTour() {
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
            steps: [
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
            ],
            onDestroyStarted: function(_, __, opts) {
                // Called when user clicks X, overlay, or Done on last step.
                // Driver.js does not bind `this`, so use opts.driver.
                markComplete();
                if (opts && opts.driver && typeof opts.driver.destroy === 'function') {
                    opts.driver.destroy();
                }
            }
        });
    }

    // waitForTourTargets waits for the first tour step's target element to
    // exist in the DOM before resolving. Uses a MutationObserver with a
    // fallback timeout so the tour never hangs indefinitely.
    function waitForTourTargets(timeoutMs) {
        var TARGET_SELECTOR = '#sw-sidebar';
        return new Promise(function(resolve) {
            if (document.querySelector(TARGET_SELECTOR)) {
                resolve();
                return;
            }
            var resolved = false;
            var observer = new MutationObserver(function() {
                if (!resolved && document.querySelector(TARGET_SELECTOR)) {
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

    // Mark the tour as pending so it auto-starts on the next Artists page load.
    // Centralizes localStorage key management so other templates (e.g. onboarding)
    // do not need to reference key names directly.
    window.markTourPending = function() {
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        localStorage.setItem(TOUR_PENDING_KEY, 'true');
    };

    // Public API for manual restart from guide page or help overlay.
    window.startGuidedTour = function() {
        var bp = basePath();
        var artistsPath = bp + '/artists';
        // If not on the Artists page, navigate there first.
        if (!isArtistsPage()) {
            window.markTourPending();
            window.location.href = artistsPath;
            return;
        }
        // Already on Artists page -- start immediately.
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        var tour = createTour();
        tour.drive();
    };

    // Auto-start: wait for tour target elements to appear, then allow a brief
    // grace period for HTMX to hydrate/bind event handlers before driving.
    if (shouldAutoStart()) {
        waitForTourTargets(3000).then(function() {
            setTimeout(function() {
                var tour = createTour();
                tour.drive();
            }, 500);
        });
    }
})();
