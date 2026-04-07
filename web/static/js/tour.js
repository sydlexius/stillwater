// Guided Tour -- Driver.js step definitions and trigger logic.
// Loaded globally via layout.templ. Only executes on the Artists page
// (auto-start) or when manually invoked via startGuidedTour().
(function() {
    'use strict';

    var TOUR_COMPLETED_KEY = 'tour.completed';
    var TOUR_PENDING_KEY = 'tour.pending';

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
                        title: 'Navigation',
                        description: 'Use the sidebar to switch between Dashboard, Artists, Reports, and Settings. Collapse it with the arrow at the bottom for more space.',
                        side: 'right',
                        align: 'start'
                    }
                },
                {
                    element: '#scan-btn',
                    popover: {
                        title: 'Scan Your Library',
                        description: 'Click here to scan your music folders. Stillwater will discover artists and fetch their metadata automatically.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-search',
                    popover: {
                        title: 'Search Artists',
                        description: 'Type a name to instantly filter your artist list.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-filter-trigger',
                    popover: {
                        title: 'Filter Artists',
                        description: 'Narrow results by metadata status, image presence, artist type, or library.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#sort-dropdown',
                    popover: {
                        title: 'Sort Artists',
                        description: 'Reorder by name, health score, date added, or last updated.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#view-toggle',
                    popover: {
                        title: 'Switch Views',
                        description: 'Toggle between a detailed table view and a visual grid view.',
                        side: 'bottom'
                    }
                },
                {
                    element: '#artist-content',
                    popover: {
                        title: 'Your Artist List',
                        description: 'Your artists appear here after scanning. Click any artist to view and edit their metadata, images, and platform connections.',
                        side: 'top'
                    }
                }
            ],
            onDestroyStarted: function() {
                // Called when user clicks X or overlay. Mark complete and destroy.
                markComplete();
                if (!this.hasNextStep()) {
                    this.destroy();
                }
                this.destroy();
            },
            onDestroyed: function() {
                markComplete();
            }
        });
    }

    // Public API for manual restart from guide page or help overlay.
    window.startGuidedTour = function() {
        var bp = basePath();
        var artistsPath = bp + '/artists';
        // If not on the Artists page, navigate there first.
        if (!isArtistsPage()) {
            // Set pending so tour auto-starts on arrival.
            localStorage.removeItem(TOUR_COMPLETED_KEY);
            localStorage.setItem(TOUR_PENDING_KEY, 'true');
            window.location.href = artistsPath;
            return;
        }
        // Already on Artists page -- start immediately.
        localStorage.removeItem(TOUR_COMPLETED_KEY);
        var tour = createTour();
        tour.drive();
    };

    // Auto-start check on page load.
    if (shouldAutoStart()) {
        // Delay to let HTMX-loaded content settle (toolbar elements are
        // server-rendered, but artist list loads via hx-trigger="load").
        setTimeout(function() {
            var tour = createTour();
            tour.drive();
        }, 500);
    }
})();
