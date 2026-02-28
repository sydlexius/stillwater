// Help overlay search and navigation.
// Loaded once in layout.templ; depends on #help-overlay and #help-data from help_overlay.templ.
(function() {
    'use strict';

    var sections = [];
    var overlay = null;

    function init() {
        var dataEl = document.getElementById('help-data');
        if (dataEl) {
            try { sections = JSON.parse(dataEl.textContent); } catch(e) { sections = []; }
        }
    }

    function getOverlay() {
        if (!overlay) {
            overlay = document.getElementById('help-overlay');
        }
        return overlay;
    }

    function isOpen() {
        var el = getOverlay();
        return el && !el.classList.contains('hidden');
    }

    window.toggleHelpOverlay = function() {
        var el = getOverlay();
        if (!el) return;
        if (isOpen()) {
            el.classList.add('hidden');
        } else {
            if (sections.length === 0) init();
            el.classList.remove('hidden');
            var input = document.getElementById('help-search-input');
            if (input) {
                input.value = '';
                input.focus();
            }
            renderHelpResults(getContextResults());
        }
    };

    window.searchHelp = function(query) {
        if (sections.length === 0) init();
        query = (query || '').trim().toLowerCase();
        if (!query) {
            renderHelpResults(getContextResults());
            return;
        }

        var words = query.split(/\s+/);
        var scored = sections.map(function(s) {
            var score = 0;
            var titleLower = s.title.toLowerCase();
            var summaryLower = s.summary.toLowerCase();
            for (var i = 0; i < words.length; i++) {
                var w = words[i];
                if (titleLower.indexOf(w) !== -1) score += 10;
                if (summaryLower.indexOf(w) !== -1) score += 5;
                for (var j = 0; j < s.keywords.length; j++) {
                    if (s.keywords[j].indexOf(w) !== -1) score += 3;
                }
            }
            if (isPageMatch(s.pages)) score += 2;
            return { section: s, score: score };
        });

        scored.sort(function(a, b) { return b.score - a.score; });

        var results = [];
        for (var i = 0; i < scored.length; i++) {
            if (scored[i].score > 0) results.push(scored[i].section);
        }
        renderHelpResults(results);
    };

    function renderHelpResults(results) {
        var container = document.getElementById('help-results');
        if (!container) return;

        if (results.length === 0) {
            container.innerHTML = '<p class="text-sm text-gray-500 dark:text-gray-400 italic">No matching help topics.</p>';
            return;
        }

        var html = '';
        for (var i = 0; i < results.length; i++) {
            var s = results[i];
            html += '<a href="/guide#' + escapeHtml(s.id) + '" class="block rounded-lg border border-gray-200 dark:border-gray-700 p-3 hover:bg-gray-50 dark:hover:bg-gray-700/50 transition-colors">'
                + '<h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">' + escapeHtml(s.title) + '</h4>'
                + '<p class="text-xs text-gray-500 dark:text-gray-400 mt-0.5">' + escapeHtml(s.summary) + '</p>'
                + '</a>';
        }
        container.innerHTML = html;
    }

    function isPageMatch(pages) {
        var path = window.location.pathname;
        for (var i = 0; i < pages.length; i++) {
            var p = pages[i];
            if (p.charAt(p.length - 1) === '/') {
                // Prefix match for paths ending with /
                if (path.indexOf(p) === 0) return true;
            } else {
                if (path === p) return true;
            }
        }
        return false;
    }

    function getContextResults() {
        var matched = [];
        var unmatched = [];
        for (var i = 0; i < sections.length; i++) {
            if (isPageMatch(sections[i].pages)) {
                matched.push(sections[i]);
            } else {
                unmatched.push(sections[i]);
            }
        }
        return matched.concat(unmatched);
    }

    function escapeHtml(str) {
        var div = document.createElement('div');
        div.appendChild(document.createTextNode(str));
        return div.innerHTML;
    }

    // Close on backdrop click
    document.addEventListener('click', function(e) {
        if (e.target && e.target.id === 'help-backdrop') {
            toggleHelpOverlay();
        }
    });

    // Close on Escape
    document.addEventListener('keydown', function(e) {
        if (e.key === 'Escape' && isOpen()) {
            toggleHelpOverlay();
        }
    });
})();
