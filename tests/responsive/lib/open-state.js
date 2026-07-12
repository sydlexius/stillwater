// open-state.js - drives an interactive affordance (drawer, sheet, menu) into
// its OPEN state before the probes measure it (issue #2386 fix-round-3).
//
// THE BUG THIS FIXES. The pre-fix code was:
//
//     await trigger.click({ timeout: 5_000 });
//     await page.waitForSelector(open.waitFor, { timeout: 8_000 }).catch(() => {});
//
// That `.catch(() => {})` is the cardinal sin, armed and waiting. If the click
// SUCCEEDS but the drawer never opens -- a JS error, a class rename, a race --
// the wait rejects, the rejection is swallowed, NO marker is set anywhere, and
// the record is written as a clean, successful measurement of the OPEN state
// that is in fact the CLOSED-state page. The only reason it had not fired yet
// is the accident that the trigger is currently a 0x0 element, so `click()`
// throws first and the outer catch does set a marker. The moment #2382 makes
// that trigger clickable, this becomes a live silent failure with no marker in
// the report.
//
// So: every failure path here sets an explicit marker on the record. There is
// no silent branch.
//
// THE SECOND BUG. Even a genuine open was being probed MID-ANIMATION.
// web/static/js/prefs-drawer.js sets aria-hidden=false at the START of a 0.25s
// slide-in transition (web/static/css/input.css), and `waitFor` matches on
// exactly that attribute -- so the probes ran while the drawer was still
// partway off-canvas, and the off-screen probe duly measured a half-open
// drawer. waitForOpenAndSettle() therefore waits for the open state AND for the
// element's geometry to stop changing.

// waitForStableRect resolves once `selector`'s bounding box has been identical
// across `frames` consecutive animation frames -- i.e. the transition has
// finished, whatever its duration or easing. Cheaper and more robust than
// hard-coding a sleep to the CSS transition duration (which drifts the moment
// someone retunes the animation) and than transitionend (which never fires if
// the transition is interrupted or if prefers-reduced-motion removes it).
async function waitForStableRect(page, selector, { frames = 3, timeout = 3_000 } = {}) {
  return page.evaluate(({ selector, frames, timeout }) => new Promise(resolve => {
    const started = Date.now();
    let last = null;
    let stable = 0;
    const tick = () => {
      const el = document.querySelector(selector);
      if (!el) return resolve({ stable: false, reason: 'element vanished while settling' });
      const r = el.getBoundingClientRect();
      const key = `${r.left},${r.top},${r.width},${r.height}`;
      stable = key === last ? stable + 1 : 0;
      last = key;
      if (stable >= frames) return resolve({ stable: true, waitedMs: Date.now() - started });
      if (Date.now() - started > timeout) {
        return resolve({ stable: false, reason: `geometry still changing after ${timeout}ms` });
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }), { selector, frames, timeout });
}

// openAffordance clicks `openDef.trigger` and waits for `openDef.waitFor`, then
// for the resulting element to stop moving. It never throws: every outcome is
// written onto `record` as an explicit marker, because the caller (run.js) must
// keep measuring the rest of the matrix even when one affordance misbehaves.
//
// Markers, all of which mean "the numbers below are NOT open-state numbers":
//   record.openSkipped -- the trigger is not in the DOM at all
//   record.openFailed  -- the trigger exists but the open state never arrived
//   record.openUnsettled -- it opened, but its geometry never stopped moving
//                           (the probes ran mid-animation; treat with suspicion)
export async function openAffordance(page, openDef, record) {
  const closedName = record.page.replace(/-open$/, '');
  const trigger = page.locator(openDef.trigger).first();

  if (await trigger.count() === 0) {
    record.openSkipped = `trigger "${openDef.trigger}" not found in the DOM`;
    return;
  }

  // A present-but-not-actionable trigger (zero-size, covered, unclickable) is
  // itself a real finding worth keeping -- NOT a reason to let the whole
  // viewport/theme run crash. Bound the click well under Playwright's 30s
  // action-timeout default: discovered live (#2386 dogfooding) that this exact
  // trigger is a 0x0 element at all three pinned viewports on /next/, and an
  // uncaught click timeout took down the entire browser context, silently
  // losing every later page/theme combination in that context's loop.
  try {
    await trigger.click({ timeout: 5_000 });
  } catch (err) {
    record.openFailed = `trigger "${openDef.trigger}" present but not actionable `
      + `(zero-size/covered/unclickable: ${firstLine(err)}) -- the probes below ran `
      + `against the CLOSED-state page, not the open ${closedName}`;
    return;
  }

  // The click landed. If the open state does not follow, that is a REAL BUG in
  // the app (or in this selector) and it gets a marker. It does not get a
  // `.catch(() => {})`.
  try {
    await page.waitForSelector(openDef.waitFor, { timeout: 8_000 });
  } catch (err) {
    record.openFailed = `trigger "${openDef.trigger}" clicked successfully but the open `
      + `state "${openDef.waitFor}" never appeared within 8s (${firstLine(err)}) -- the `
      + `probes below ran against the CLOSED-state page, not the open ${closedName}`;
    return;
  }

  const settled = await waitForStableRect(page, openDef.waitFor);
  if (!settled.stable) {
    record.openUnsettled = `open state "${openDef.waitFor}" appeared but never settled `
      + `(${settled.reason}) -- the probes below may have measured it mid-transition`;
    return;
  }
  record.openSettledMs = settled.waitedMs;
}

function firstLine(err) {
  return String(err && err.message || err).split('\n')[0];
}
