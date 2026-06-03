// Settings: extracted from the inline updatesTabScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-exports of the handlers called by bare name from the Updates-tab markup,
// and the single CSRF read (the getCSRFToken() helper) routed through the
// canonical window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex. This commit completes #1808's param-less script
// extraction: no inline XxxScript() blocks remain in settings.templ.
//
// DOM contract (Updates tab; targets [data-tab-panel="updates"]):
//   Buttons (bare-name onclick handlers in settings.templ):
//     onclick="checkForUpdates()"  -- #updates-check-btn
//     onclick="applyUpdate()"      -- #updates-apply-btn
//     onclick="skipThisVersion()"  -- #updates-skip-version-btn (data-version)
//   Auto-save controls (wired via change / segmented:changed -> saveUpdaterConfig):
//     #updates-channel (segmented, name="updates_channel"), #updates-enabled,
//     #updates-auto-check, #updates-auto-update, #updates-check-interval.
//   Status / banners:
//     #updates-status-row (data-updaterState), #updates-status-spinner,
//     #updates-status-text, #updates-error-row,
//     #updates-last-checked-row, #updates-last-checked,
//     #updates-latest-version, #updates-release-link-row, #updates-release-link,
//     #updates-restart-required-row, #updates-restart-pending-version,
//     #updates-load-failed-row (data-load-failed),
//     #updates-i18n (data-* translated labels),
//     #updates-auto-update-confirm-modal / -accept / -cancel.
//   Tab activation: [data-tab="updates"] click + DOMContentLoaded + popstate.
//   Dispatches the 'sw:update-status-changed' CustomEvent for the sidebar pill.
// Network (all base-path aware, csrf_token sent as X-CSRF-Token):
//   POST {base}/api/v1/updates/check
//   POST {base}/api/v1/updates/apply
//   GET  {base}/api/v1/updates/status
//   GET  {base}/api/v1/updates/config   (revertUpdatesUIFromConfig)
//   PUT  {base}/api/v1/updates/config   (saveUpdaterConfig)
//   POST {base}/api/v1/updates/skips    (skipThisVersion)
//
// Export surface: window.swUpdates doubles as the load-once guard. The four
// HTML-/listener-referenced handlers (checkForUpdates, applyUpdate,
// saveUpdaterConfig, skipThisVersion) are assigned to window inside the body,
// preserved verbatim; everything else stays internal to this IIFE.
(function() {
	'use strict';

	if (window.swUpdates) return;
	window.swUpdates = true;

	var updBp = '';
	var updMeta = document.querySelector('meta[name="htmx-base-path"]');
	if (updMeta && updMeta.content) {
		updBp = updMeta.content;
	}

	// restartRequiredActive reports whether the persistent
	// "restart required" banner is currently visible. Used to gate
	// updater actions that would otherwise re-surface stale state
	// against the still-running old version.
	function restartRequiredActive() {
		var row = document.getElementById('updates-restart-required-row');
		return !!(row && !row.classList.contains('hidden'));
	}

	// checkForUpdates calls the check endpoint and refreshes the UI.
	window.checkForUpdates = function() {
		// After Apply staged the new binary, /check would re-run
		// against the old in-memory version and re-surface the same
		// "update available" pill the restart-required flow is
		// suppressing. Skip until restart finishes (issue #1169).
		if (restartRequiredActive()) return;
		var btn = document.getElementById('updates-check-btn');
		if (btn) btn.disabled = true;

		fetch(updBp + '/api/v1/updates/check', {
			method: 'POST',
			headers: {'Accept': 'application/json', 'X-CSRF-Token': getCSRFToken()}
		})
		.then(function(resp) {
			if (!resp.ok) return throwFromResponse(resp, 'check failed');
			return resp.json();
		})
		.then(function(data) {
			// /check mutates last_checked and the cached release fields, so
			// re-hydrate from /status to keep every updater DOM node
			// (last-checked timestamp, latest version badge, release-notes
			// row, apply button) consistent with a single code path.
			clearUpdaterStatus();
			// Passive refresh after /check: a /status failure must
			// not bring back the inline status row this PR retires.
			fetchAndPopulateUpdateStatus({silent: true});
			// Notify other page components (sidebar pill/dot) that the
			// cached updater status has changed so they can re-read it.
			document.dispatchEvent(new CustomEvent('sw:update-status-changed'));
			// Surface the check result via the canonical toast so it
			// matches the pattern used elsewhere in Settings.
			if (typeof window.showSuccessToast === 'function') {
				var i18nEl = document.getElementById('updates-i18n');
				var upMsg = (i18nEl && i18nEl.dataset.upToDate) || 'Up to date';
				var availMsg = (i18nEl && i18nEl.dataset.updateAvailable) || 'Update available';
				if (data && data.update_available) {
					showSuccessToast(data.latest ? availMsg + ': ' + data.latest : availMsg);
				} else {
					showSuccessToast(upMsg);
				}
			}
		})
		.catch(function(err) {
			clearUpdaterStatus();
			if (typeof window.showToast === 'function') {
				showToast((err && err.message) || 'Failed to check for updates');
			}
		})
		.finally(function() {
			if (btn) btn.disabled = false;
		});
	};

	// applyUpdate calls the apply endpoint.
	window.applyUpdate = function() {
		var btn = document.getElementById('updates-apply-btn');
		if (btn) btn.disabled = true;
		setUpdaterStatus('applying', '');

		fetch(updBp + '/api/v1/updates/apply', {
			method: 'POST',
			headers: {'Accept': 'application/json', 'X-CSRF-Token': getCSRFToken()}
		})
		.then(function(resp) {
			if (resp.status === 422) return throwFromResponse(resp, 'not supported');
			if (resp.status === 409) return throwFromResponse(resp, 'already in progress');
			if (!resp.ok) return throwFromResponse(resp, 'apply failed');
			return resp.json();
		})
		.then(function() {
			setUpdaterStatus('downloading', '');
			pollUpdateStatus();
		})
		.catch(function(err) {
			setUpdaterStatus('error', err.message);
			if (btn) btn.disabled = false;
		});
	};

	// updaterPollTimer is the single active poll interval. Only one poller
	// may run at a time; reopening the Updates tab during an apply would
	// otherwise stack multiple intervals, each racing to clear state.
	var updaterPollTimer = null;

	// persistedUpdatesEnabled tracks the last server-confirmed value of
	// the updater kill switch. The Apply button must be gated on this
	// rather than the live #updates-enabled checkbox state, because POST
	// /api/v1/updates/apply is enforced from the persisted config: if
	// the user toggles the checkbox without clicking Save, the server
	// still 403s an Apply request, but a checkbox-driven gate would
	// happily re-enable the button on the next /status hydrate.
	//
	// Seeded at script load from the server-rendered checkbox state
	// (which IS the persisted value at render time), then updated only
	// when saveUpdaterConfig completes successfully. Defaults to true
	// when the checkbox is absent so the prior behaviour holds for
	// hydrate paths that fire before the form has fully rendered.
	var persistedUpdatesEnabled = (function() {
		var el = document.getElementById('updates-enabled');
		return el ? el.checked : true;
	})();

	// updatesSaveSeq + updatesSaveAbort sequence overlapping auto-saves.
	// Auto-save fires on every channel/checkbox/interval change, so a
	// rapid toggle (stable -> prerelease -> nightly) issues overlapping
	// PUTs whose responses can land out of order. Without sequencing,
	// the older response can run its post-save UI updates after the
	// newer one, snapping the UI back to a stale value while the server
	// holds the latest. The AbortController cancels the previous in-
	// flight request on each new save; the saveSeq compare ensures any
	// late .then/.catch from a superseded request no-ops instead of
	// clobbering the current state.
	var updatesSaveSeq = 0;
	var updatesSaveAbort = null;

	// computeApplyDisabled is the single source of truth for the Apply
	// button's disabled state on every /status hydrate. It mirrors the
	// server-side rejection paths in POST /api/v1/updates/apply
	// (handlers_updater.go) so the UI never invites a click that can
	// only return 403 or 409. Keeping the predicate centralised here
	// (rather than inlined per render branch) prevents the recurring
	// pattern where a new server-side rejection condition gets added
	// or rediscovered and the per-branch checks fall out of sync.
	//
	// Server-side rejection map (must stay in lockstep):
	//   - IsDocker:           422  -> NOT checked here; the Apply button
	//                                 is not rendered at all in Docker
	//                                 mode (server-side branch in the
	//                                 updatesTab template), so applyBtn
	//                                 is null and every caller already
	//                                 guards on `if (applyBtn)`.
	//   - GetConfig errors:   500  -> transient server error; not
	//                                 reflected in /status payload, so
	//                                 not gateable client-side.
	//   - !cfg.Enabled:       403  -> persistedUpdatesEnabled.
	//   - ErrAlreadyRunning:  409  -> d.state && d.state !== 'idle'.
	//   - ErrRestartRequired: 409  -> d.restart_required.
	//
	// Plus two UX gates with no server analogue (the server would
	// happily 200 a no-op apply, but a clickable button advertising
	// "apply nothing" is just confusing):
	//   - !d.update_available: nothing newer than the running build.
	//   - !d.latest:           no successful check has populated a
	//                          target version yet.
	function computeApplyDisabled(d) {
		// Fail closed when the render-time read failed: every
		// updater control was seeded from in-code defaults rather
		// than the user's real config, so trusting them to enable
		// Apply would invite a click that the server rejects on a
		// stale-state mirror miss.
		if (updatesLoadFailed()) return true;
		if (!persistedUpdatesEnabled) return true;
		if (!d.update_available || !d.latest) return true;
		if (d.restart_required) return true;
		if (d.state && d.state !== 'idle') return true;
		return false;
	}

	function stopUpdaterPoll() {
		if (updaterPollTimer !== null) {
			clearInterval(updaterPollTimer);
			updaterPollTimer = null;
		}
	}

	// pollUpdateStatus polls /api/v1/updates/status until idle or error.
	// Fails closed on HTTP or parse errors: a 5xx or malformed body stops
	// the poller and surfaces the failure, otherwise the spinner would
	// spin indefinitely.
	function pollUpdateStatus() {
		if (updaterPollTimer !== null) return;
		updaterPollTimer = setInterval(function() {
			fetch(updBp + '/api/v1/updates/status', {headers: {'Accept': 'application/json'}})
			.then(function(r) {
				if (!r.ok) return throwFromResponse(r, 'status failed');
				return r.json();
			})
			.then(function(d) {
				if (d.state === 'idle') {
					stopUpdaterPoll();
					clearUpdaterStatus();
					// Distinguish post-Apply success from idle-no-op: when
					// the server reports restart_required=true, surface a
					// persistent "restart to finish" banner and stop here
					// rather than re-checking GitHub (which would re-show
					// the same "Update available" pill against the still-
					// running old version, masking the success). Issue #1169.
					if (d.restart_required) {
						renderRestartRequired(d.pending_version || '');
					} else {
						// Trigger a fresh check to show the new version.
						checkForUpdates();
					}
				} else if (d.state === 'error') {
					stopUpdaterPoll();
					setUpdaterStatus('error', d.error || 'update failed');
				} else {
					setUpdaterStatus(d.state, '');
				}
			})
			.catch(function(err) {
				stopUpdaterPoll();
				setUpdaterStatus('error', err.message || 'status failed');
			});
		}, 2000);
	}

	// renderRestartRequired flips the persistent restart-required banner
	// visible and hard-disables the Apply button. The banner is the same
	// DOM that the server may have already rendered visible (when the
	// page is loaded after a successful Apply); both code paths converge
	// on the same element so a JS-driven flip and an SSR-driven flip
	// look identical to the user.
	function renderRestartRequired(pendingVersion) {
		var row = document.getElementById('updates-restart-required-row');
		if (row) row.classList.remove('hidden');
		var pv = document.getElementById('updates-restart-pending-version');
		if (pv) {
			if (pendingVersion) {
				pv.textContent = pendingVersion;
				pv.classList.remove('hidden');
			} else {
				pv.textContent = '';
				pv.classList.add('hidden');
			}
		}
		var applyBtn = document.getElementById('updates-apply-btn');
		if (applyBtn) applyBtn.disabled = true;
		// Disable Check too so the user can't re-run /check against
		// the old in-memory version and re-surface the update pill.
		// checkForUpdates() also short-circuits via
		// restartRequiredActive(); this keeps the visual state in sync.
		var checkBtn = document.getElementById('updates-check-btn');
		if (checkBtn) checkBtn.disabled = true;
	}

	// updatesLoadFailed reports whether the server-side render flagged
	// LoadFailed. When true, every persistence path is short-circuited:
	// the rendered controls show in-code defaults (not the user's
	// real config) and a PUT would overwrite real values with those
	// defaults. The DOM marker survives a user dismissing the
	// banner because dismissal is purely visual.
	function updatesLoadFailed() {
		var row = document.getElementById('updates-load-failed-row');
		return !!(row && row.dataset.loadFailed === 'true');
	}

	// revertUpdatesUIFromConfig refetches the persisted updater config
	// and snaps the channel segmented control, enabled toggle,
	// auto-check checkbox, and check-interval select back to server
	// state. Called after a failed PUT so the optimistic UI does not
	// drift from what was actually saved (auto-save makes the user's
	// last click feel persisted; a failed PUT silently leaves the
	// drifted UI without this revert).
	function revertUpdatesUIFromConfig() {
		fetch(updBp + '/api/v1/updates/config', {headers: {'Accept': 'application/json'}})
		.then(function(resp) {
			if (!resp.ok) return null;
			return resp.json();
		})
		.then(function(cfg) {
			if (!cfg) return;
			var channelEl = document.querySelector('[name="updates_channel"][value="' + cfg.channel + '"]');
			if (channelEl) channelEl.checked = true;
			// Re-paint segmented visuals if the component exposes a hook.
			var seg = document.getElementById('updates-channel');
			if (seg && typeof seg.swSegmentedSync === 'function') {
				seg.swSegmentedSync();
			}
			var enabledEl = document.getElementById('updates-enabled');
			if (enabledEl) enabledEl.checked = !!cfg.enabled;
			var autoEl = document.getElementById('updates-auto-check');
			if (autoEl) autoEl.checked = !!cfg.auto_check;
			var autoUpdateEl2 = document.getElementById('updates-auto-update');
			if (autoUpdateEl2 && cfg.auto_update !== undefined) {
				autoUpdateEl2.checked = !!cfg.auto_update;
			}
			var intervalEl = document.getElementById('updates-check-interval');
			if (intervalEl && cfg.check_interval_hours) {
				intervalEl.value = String(cfg.check_interval_hours);
			}
		})
		.catch(function() {
			// Best effort revert; if /config itself fails, leave the
			// last-known UI state. The error toast from the original
			// PUT failure is still visible.
		});
	}

	// saveUpdaterConfig persists channel + enabled + auto-check + interval.
	// Read the checked radio, not the first radio, or Save always sends "stable".
	window.saveUpdaterConfig = function() {
		if (updatesLoadFailed()) {
			// Refuse to PUT when the render-time read failed: the
			// rendered values are defaults, not the user's real
			// config, and saving them would overwrite real settings.
			var i18nEl = document.getElementById('updates-i18n');
			var msg = (i18nEl && i18nEl.dataset.loadFailedSaveBlocked) || 'save blocked: configuration failed to load';
			if (typeof window.showToast === 'function') {
				showToast(msg);
			}
			// The change event already mutated the touched control in
			// the DOM. Snap every updater control back to the actually
			// persisted server state so the tab does not show a value
			// that cannot be saved. revertUpdatesUIFromConfig is a
			// best-effort fetch of /api/v1/updates/config; if /config
			// itself fails too, the caller-visible toast above is the
			// only signal, but the controls stay in their post-change
			// state (which is no worse than the prior behavior).
			revertUpdatesUIFromConfig();
			return;
		}
		var channelEl = document.querySelector('[name="updates_channel"]:checked');
		var enabledEl = document.getElementById('updates-enabled');
		var autoEl = document.getElementById('updates-auto-check');
		var autoUpdateEl = document.getElementById('updates-auto-update');
		var intervalEl = document.getElementById('updates-check-interval');
		var channel = channelEl ? channelEl.value : 'stable';
		var enabled = enabledEl ? enabledEl.checked : true;
		var autoCheck = autoEl ? autoEl.checked : false;
		var autoUpdate = autoUpdateEl ? autoUpdateEl.checked : false;
		var interval = intervalEl ? parseInt(intervalEl.value, 10) : 24;
		if (!interval || interval < 1) {
			interval = 24;
		}

		// Cancel any prior in-flight save and bump the sequence so
		// stale .then/.catch handlers from the superseded request can
		// detect they are no longer the current save and no-op.
		if (updatesSaveAbort) {
			updatesSaveAbort.abort();
		}
		updatesSaveAbort = new AbortController();
		var saveSeq = ++updatesSaveSeq;

		fetch(updBp + '/api/v1/updates/config', {
			method: 'PUT',
			headers: {
				'Content-Type': 'application/json',
				'X-CSRF-Token': getCSRFToken()
			},
			body: JSON.stringify({
				channel: channel,
				enabled: enabled,
				auto_check: autoCheck,
				auto_update: autoUpdate,
				check_interval_hours: interval
			}),
			signal: updatesSaveAbort.signal
		})
		.then(function(resp) {
			if (!resp.ok) return throwFromResponse(resp, 'save failed');
			return resp.json();
		})
		.then(function() {
			// Skip post-save UI updates if a newer save has superseded
			// this one. The newer save's handlers will run shortly with
			// up-to-date values; running these now would briefly paint
			// stale state.
			if (saveSeq !== updatesSaveSeq) {
				return;
			}
			// Save succeeded: the value the user just submitted is now the
			// persisted state, so the Apply gate (consulted by the upcoming
			// fetchAndPopulateUpdateStatus call) reflects the correct
			// server-side authority on the next hydrate.
			persistedUpdatesEnabled = enabled;

			// Channel changes can invalidate the cached latest/release_url
			// (new channel may have no matching release), so re-hydrate the
			// updater UI from /status immediately. fetchAndPopulateUpdateStatus
			// drives last-checked, latest, release-notes row, and the Apply
			// button through the single /status code path so nothing stays
			// stale in the DOM.
			var savedI18n = document.getElementById('updates-i18n');
			var savedMsg = (savedI18n && savedI18n.dataset.saved) || 'Settings saved';
			if (typeof window.showSuccessToast === 'function') {
				showSuccessToast(savedMsg);
			}
			// Passive refresh after save: a /status failure must not
			// resurface the inline status row.
			fetchAndPopulateUpdateStatus({silent: true});
			// Notify the sidebar pill/dot: the server cleared its cached
			// release fields if the channel actually changed, so the
			// sidebar must re-read /status too.
			document.dispatchEvent(new CustomEvent('sw:update-status-changed'));
		})
		.catch(function(err) {
			// AbortError fires when a newer save cancelled this one; it
			// is not a real failure, the newer save will surface its own
			// outcome.
			if (err && err.name === 'AbortError') {
				return;
			}
			// A superseded save's network failure is also irrelevant:
			// the newer save's outcome is what the user cares about.
			if (saveSeq !== updatesSaveSeq) {
				return;
			}
			if (typeof window.showToast === 'function') {
				showToast((err && err.message) || 'Failed to save updater settings');
			}
			// Revert optimistic UI: with the Save button gone, an
			// auto-save event already painted the new control state.
			// On failure we re-read /config and snap controls back to
			// the persisted values so the rendered state matches what
			// is actually saved on the server.
			revertUpdatesUIFromConfig();
		});
	};

	function setUpdaterStatus(state, errMsg) {
		var row = document.getElementById('updates-status-row');
		var spinner = document.getElementById('updates-status-spinner');
		var text = document.getElementById('updates-status-text');
		var errRow = document.getElementById('updates-error-row');
		if (!row) return;

		row.classList.remove('hidden');
		// Record the active state so downstream timeouts can check
		// whether they still own the DOM before clearing it (prevents
		// a delayed "saved" cleanup from wiping a subsequent "error"
		// toast set by a concurrent /status fetch).
		row.dataset.updaterState = state;
		if (errMsg) {
			errRow.textContent = errMsg;
			errRow.classList.remove('hidden');
		} else {
			errRow.classList.add('hidden');
			errRow.textContent = '';
		}

		// Read translated labels from the data-* attributes on the i18n element.
		var i18n = document.getElementById('updates-i18n');
		var labels = i18n ? i18n.dataset : {};
		var label = labels[state] || state;
		text.textContent = label;

		var inProgress = state === 'checking' || state === 'downloading' || state === 'applying';
		if (spinner) {
			if (inProgress) spinner.classList.remove('hidden');
			else spinner.classList.add('hidden');
		}
	}

	function clearUpdaterStatus() {
		var row = document.getElementById('updates-status-row');
		if (row) {
			row.classList.add('hidden');
			delete row.dataset.updaterState;
		}
	}

	function escHtml(s) {
		return String(s)
			.replace(/&/g, '&amp;')
			.replace(/</g, '&lt;')
			.replace(/>/g, '&gt;')
			.replace(/"/g, '&quot;');
	}

	function getCSRFToken() {
		return (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
	}

	// Parse a non-ok fetch response's JSON body and throw a useful error,
	// tolerating non-JSON bodies (e.g. proxy HTML 502s) that would otherwise
	// crash resp.json() with a SyntaxError before any user-visible message.
	function throwFromResponse(resp, fallback) {
		return resp.json().catch(function() { return {}; }).then(function(d) {
			throw new Error(d.error || fallback || ('HTTP ' + resp.status));
		});
	}

	// Shared hydrator used on DOMContentLoaded and popstate so browser
	// back/forward navigation also refreshes the panel instead of leaving
	// stale DOM behind when the user returns to the tab.
	function hydrateIfUpdatesActive() {
		var panel = document.querySelector('[data-tab-panel="updates"]');
		if (panel && !panel.classList.contains('hidden')) {
			fetchAndPopulateUpdateStatus();
		}
	}

	document.addEventListener('DOMContentLoaded', function() {
		var tabLinks = document.querySelectorAll('[data-tab="updates"]');
		// Array.prototype.forEach.call rather than NodeList.forEach: the repo
		// targets ES5, where NodeList.forEach is not guaranteed.
		Array.prototype.forEach.call(tabLinks, function(link) {
			link.addEventListener('click', function() {
				fetchAndPopulateUpdateStatus();
			});
		});
		hydrateIfUpdatesActive();

		// Auto-save wiring: every Updates-tab control persists on
		// change. The Segmented component dispatches a custom
		// 'segmented:changed' event with detail.name; we filter on
		// 'updates_channel' so adding another segmented control on
		// this page in the future does not accidentally trigger a
		// PUT. Native checkboxes and the interval <select> use the
		// standard 'change' event. saveUpdaterConfig itself short
		// circuits when LoadFailed is set, so dismissing the banner
		// cannot reach the destructive write path.
		document.addEventListener('segmented:changed', function(e) {
			if (e && e.detail && e.detail.name === 'updates_channel') {
				window.saveUpdaterConfig();
			}
		});
		var enabledEl = document.getElementById('updates-enabled');
		if (enabledEl) {
			enabledEl.addEventListener('change', function() { window.saveUpdaterConfig(); });
		}
		var autoEl = document.getElementById('updates-auto-check');
		if (autoEl) {
			autoEl.addEventListener('change', function() { window.saveUpdaterConfig(); });
		}
		var intervalEl = document.getElementById('updates-check-interval');
		if (intervalEl) {
			intervalEl.addEventListener('change', function() { window.saveUpdaterConfig(); });
		}

		// AutoUpdate toggle: confirm on off->on transition. The
		// confirmed state is persisted in localStorage under
		// "ui.confirm.autoUpdate" so a page reload does not re-prompt
		// after the user has already acknowledged. Existing
		// AutoUpdate=true (e.g. a server-side default or a prior
		// session) also counts as confirmed so the modal does not
		// pop on every load. The "Reset confirmation preferences"
		// flow can clear this key to replay the modal.
		var autoUpdateEl = document.getElementById('updates-auto-update');
		var autoUpdateConfirmKey = 'ui.confirm.autoUpdate';
		var readAutoUpdateConfirmed = function() {
			try {
				return window.localStorage.getItem(autoUpdateConfirmKey) === 'true';
			} catch (_e) {
				// Some sandboxed contexts (private mode, restrictive
				// CSP) deny localStorage access; fall back to the
				// page-local boolean for the current session.
				return false;
			}
		};
		var writeAutoUpdateConfirmed = function() {
			try {
				window.localStorage.setItem(autoUpdateConfirmKey, 'true');
			} catch (_e) {
				// Best-effort: a write failure just means the modal
				// will re-prompt on next load.
			}
		};
		// Treat an already-on toggle at page load as confirmed so we
		// do not re-prompt on every reload after the user accepted.
		var autoUpdateConfirmed = readAutoUpdateConfirmed() || (autoUpdateEl ? autoUpdateEl.checked : false);
		if (autoUpdateConfirmed) {
			writeAutoUpdateConfirmed();
		}
		if (autoUpdateEl) {
			autoUpdateEl.addEventListener('change', function() {
				// Off -> on transition that has not been confirmed
				// previously: surface the modal and short-circuit
				// the save until the user accepts.
				if (autoUpdateEl.checked && !autoUpdateConfirmed) {
					var modal = document.getElementById('updates-auto-update-confirm-modal');
					if (!modal) {
						// Defensive: if the modal is missing for any
						// reason, fall back to the old direct-save path
						// rather than wedging the toggle.
						autoUpdateConfirmed = true;
						writeAutoUpdateConfirmed();
						window.saveUpdaterConfig();
						return;
					}
					modal.classList.remove('hidden');
					var accept = document.getElementById('updates-auto-update-confirm-accept');
					var cancel = document.getElementById('updates-auto-update-confirm-cancel');
					// Focus management: stash the element that had focus
					// before the modal opened (typically the checkbox
					// itself) so we can restore focus on every close path.
					// Without this, focus stays on the checkbox while the
					// modal is visible and a stray Space key re-toggles
					// the checkbox + re-fires saveUpdaterConfig().
					var previouslyFocused = document.activeElement;
					var onKeydown = function(ev) {
						if (ev.key === 'Escape' || ev.key === 'Esc') {
							ev.preventDefault();
							// Treat Escape as cancel: same path as the
							// cancel button (revert the checkbox + close).
							if (cancel) cancel.onclick && cancel.onclick();
						}
					};
					var cleanup = function() {
						modal.classList.add('hidden');
						if (accept) accept.onclick = null;
						if (cancel) cancel.onclick = null;
						document.removeEventListener('keydown', onKeydown);
						// Restore focus to the originally focused element
						// (typically the checkbox) so keyboard users land
						// back where they started rather than at <body>.
						if (previouslyFocused && typeof previouslyFocused.focus === 'function') {
							try { previouslyFocused.focus(); } catch (e) { /* ignore */ }
						}
					};
					if (accept) {
						accept.onclick = function() {
							autoUpdateConfirmed = true;
							writeAutoUpdateConfirmed();
							cleanup();
							window.saveUpdaterConfig();
						};
					}
					if (cancel) {
						cancel.onclick = function() {
							autoUpdateEl.checked = false;
							cleanup();
						};
					}
					document.addEventListener('keydown', onKeydown);
					// Move focus into the modal so Space/Enter act on the
					// accept button rather than the underlying checkbox.
					if (accept && typeof accept.focus === 'function') {
						try { accept.focus(); } catch (e) { /* ignore */ }
					}
					return;
				}
				// On -> off, or already-confirmed on -> on: persist directly.
				window.saveUpdaterConfig();
			});
		}

		// Skip-this-version button: POST the latest version tag to the
		// skip-list endpoint and reload the tab so the skipped-versions
		// list and skip button hide consistently.
		window.skipThisVersion = function() {
			var btn = document.getElementById('updates-skip-version-btn');
			if (!btn) return;
			var version = btn.dataset.version;
			if (!version) return;
			btn.disabled = true;
			fetch(updBp + '/api/v1/updates/skips', {
				method: 'POST',
				headers: {
					'Content-Type': 'application/json',
					'Accept': 'application/json',
					'X-CSRF-Token': getCSRFToken()
				},
				body: JSON.stringify({ version: version })
			})
			.then(function(resp) {
				if (!resp.ok) return throwFromResponse(resp, 'skip failed');
				return resp.json();
			})
			.then(function() {
				// Reload so the SSR'd skipped-list row picks up the
				// new entry without a separate hydrate path.
				window.location.reload();
			})
			.catch(function(err) {
				btn.disabled = false;
				if (typeof window.showToast === 'function') {
					showToast((err && err.message) || 'Failed to skip version');
				}
			});
		};
	});

	// popstate fires on browser back/forward. If that navigation leaves
	// the Updates tab active, rehydrate rather than showing whatever
	// stale values were in the DOM before the back/forward event.
	window.addEventListener('popstate', hydrateIfUpdatesActive);

	// fetchAndPopulateUpdateStatus refreshes the updater DOM from /status.
	// Pass {silent: true} for passive follow-up calls (after Check or
	// Save) where a /status failure should NOT resurface the inline
	// status row. Without that, a transient /status hiccup brings back
	// the non-Apply use of the row that this PR is retiring.
	function fetchAndPopulateUpdateStatus(opts) {
		var silent = !!(opts && opts.silent);
		fetch(updBp + '/api/v1/updates/status', {headers: {'Accept': 'application/json'}})
		.then(function(r) {
			if (!r.ok) return throwFromResponse(r, 'status failed');
			return r.json();
		})
		.then(function(d) {
			var lcRow = document.getElementById('updates-last-checked-row');
			var lcEl = document.getElementById('updates-last-checked');
			if (lcEl) {
				if (d.last_checked) {
					lcEl.textContent = d.last_checked;
					if (lcRow) lcRow.classList.remove('hidden');
				} else {
					// Clear stale display when the server has no record, e.g. after a
					// channel switch to one that was never checked.
					lcEl.textContent = '';
					if (lcRow) lcRow.classList.add('hidden');
				}
			}
			// Hydrate the latest version and update-available badge from cached status.
			var latestEl = document.getElementById('updates-latest-version');
			var applyBtn = document.getElementById('updates-apply-btn');
			var i18n = document.getElementById('updates-i18n');
			var lblAvailable = i18n ? i18n.dataset.updateAvailable : 'Update available';
			var lblUpToDate = i18n ? i18n.dataset.upToDate : 'Up to date';
			var lblNotChecked = i18n ? i18n.dataset.notChecked : 'Not yet checked';
			// The latestEl branches below are display-only: they render
			// the version pill / "up to date" / "not checked" copy. The
			// Apply button's disabled state is computed from the full
			// server-rejection mirror in computeApplyDisabled(d) and
			// applied once after this block, so the per-branch render
			// no longer has to worry about getting the gate right.
			if (latestEl) {
				if (d.latest) {
					if (d.update_available) {
						latestEl.innerHTML = '<span class="text-green-600 dark:text-green-400 font-semibold">' + escHtml(d.latest) + '</span>'
							+ ' <span class="ml-2 inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-300">' + escHtml(lblAvailable) + '</span>';
					} else {
						latestEl.innerHTML = escHtml(d.latest) + ' <span class="ml-2 text-gray-400 dark:text-gray-500">' + escHtml(lblUpToDate) + '</span>';
					}
				} else if (!d.last_checked) {
					// No check has ever succeeded: keep the server-rendered
					// "Not checked" placeholder so the first /status hydrate
					// does not blank it out.
					latestEl.innerHTML = '<span class="text-gray-400 dark:text-gray-500 italic">' + escHtml(lblNotChecked) + '</span>';
				} else {
					// Checked, but the current channel has no matching release
					// (e.g. after switching to a channel with no builds yet).
					// Leave blank rather than "Not checked" since the check did happen.
					latestEl.textContent = '';
				}
			}
			// Single canonical Apply gate. Mirrors POST /updates/apply's
			// rejection paths via computeApplyDisabled (declared above
			// with the server-side parity map). Setting this once after
			// the display branches means a future server-side rejection
			// condition only needs an update in computeApplyDisabled,
			// not in N branches here.
			if (applyBtn) applyBtn.disabled = computeApplyDisabled(d);
			// Hydrate the release-notes link from /status so it is visible
			// after a page reload or tab re-open without a manual check.
			var relLinkRow = document.getElementById('updates-release-link-row');
			var relLink = document.getElementById('updates-release-link');
			if (relLinkRow && relLink) {
				if (d.release_url) {
					relLink.href = d.release_url;
					relLinkRow.classList.remove('hidden');
				} else {
					relLinkRow.classList.add('hidden');
				}
			}
			// Hydrate the persistent restart-required banner. Because the
			// flag is sticky in-memory on the server, every /status hit
			// after a successful Apply re-confirms restart_required=true,
			// so a page refresh or back/forward navigation does not lose
			// the success indicator. Issue #1169.
			var restartRow = document.getElementById('updates-restart-required-row');
			if (restartRow) {
				if (d.restart_required) {
					renderRestartRequired(d.pending_version || '');
				} else {
					restartRow.classList.add('hidden');
					// Mirror the inverse of renderRestartRequired() so a tab
					// kept open across a real restart re-enables Check (and
					// Apply, if the page-load handler doesn't reset it).
					var checkBtn = document.getElementById('updates-check-btn');
					if (checkBtn) checkBtn.disabled = false;
				}
			}
			// If an operation is in progress, show its state and start polling.
			if (d.state === 'error') {
				setUpdaterStatus('error', d.error || 'update failed');
			} else if (d.state && d.state !== 'idle') {
				setUpdaterStatus(d.state, '');
				pollUpdateStatus();
			} else {
				clearUpdaterStatus();
			}
		})
		.catch(function(err) {
			if (silent) {
				// Passive refresh: surface a toast (if available) and
				// leave the inline status row alone.
				if (typeof window.showToast === 'function') {
					showToast(err && err.message ? err.message : 'status refresh failed');
				}
				return;
			}
			setUpdaterStatus('error', err.message || 'status failed');
		});
	}
})();
