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
// TWO KINDS OF "IT DIDN'T OPEN", AND THEY ARE NOT THE SAME KIND (#2386
// fix-round-5). Both used to set the SAME marker (`openFailed`), which collapsed
// a finding ABOUT THE APP into a failure OF THE HARNESS:
//
//   * The trigger is present but NOT ACTIONABLE -- 0x0, covered, unclickable.
//     That is the app being broken, and it is precisely what this harness exists
//     to FIND. It is REPORTED (openBlocked) and does NOT fail the run. Right now
//     [data-sw-prefs-trigger] is a 0x0 element at every pinned viewport --
//     that IS app bug #2382. Failing the run on it would mean every run exits 1
//     until #2382 lands, i.e. a permanently red harness, which trains everyone to
//     ignore the exit code. A measurement tool does not fail because it found the
//     thing it was pointed at.
//
//   * The trigger WAS actionable -- the click landed -- and the open state still
//     never arrived, or arrived and never stopped moving. Nothing in the app
//     explains that away: either the affordance is broken in a way the harness
//     cannot characterise, or this selector is stale. Either way the record CLAIMS
//     to be open-state numbers and is not. That is a REAL failure (openFailed /
//     openUnsettled) and it FAILS the run.
//
// The distinction is deliberate, not an accident of which branch happened to be
// hit first. It is what keeps the harness honest once #2382 fixes the trigger:
// today's blanket openFailed would go green the moment the click starts landing,
// even if the drawer never opened.
//
// Markers, all of which mean "the numbers below are NOT open-state numbers":
//   record.openSkipped   -- the trigger is not in the DOM at all (app/selector
//                           finding; recorded, does not fail the run)
//   record.openBlocked   -- the trigger is present but not actionable
//                           (APP finding, e.g. #2382; recorded, does not fail)
//   record.openFailed    -- the click LANDED and the open state never arrived
//                           (REAL failure; fails the run)
//   record.openUnsettled -- it opened but its geometry never stopped moving, so
//                           the probes ran mid-animation (REAL failure)
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
      // Count THIS observation, not just the matches after it: a fresh key is
      // one identical frame (itself), not zero. Resetting to 0 demanded
      // `frames + 1` identical frames where the contract above promises
      // `frames` -- harmless (it only ever waited one extra frame) but a lie.
      stable = key === last ? stable + 1 : 1;
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
// See the marker table above -- and note which markers fail the run and which
// are app findings that do not.
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
  // trigger is a 0x0 element at every pinned viewport on /next/, and an
  // uncaught click timeout took down the entire browser context, silently
  // losing every later page/theme combination in that context's loop.
  // THE APP-FINDING BRANCH. Playwright's click() only throws here after
  // exhausting its actionability checks (visible, stable, receives events,
  // enabled), so this is precisely "the user could not have tapped this either".
  // That is a finding ABOUT THE APP -- the thing we came to measure -- not a
  // malfunction of the harness, so it is recorded and does NOT fail the run.
  // openBlocked, not openFailed: see the marker table at the top of this file.
  // Also, geometry, so the report says WHY rather than just "unclickable".
  try {
    await trigger.click({ timeout: 5_000 });
  } catch (err) {
    record.openBlocked = `trigger "${openDef.trigger}" present but not actionable `
      + `(zero-size/covered/unclickable: ${firstLine(err)}) -- the probes below ran `
      + `against the CLOSED-state page, not the open ${closedName}`;
    record.openBlockedRect = await triggerRect(trigger);
    return;
  }

  // THE REAL-FAILURE BRANCH. The click LANDED -- the trigger was actionable, so
  // "the app is broken in the way we are here to find" does not explain this. The
  // open state simply never arrived: the affordance is broken in a way the harness
  // cannot characterise, or this selector is stale. Either way the record would
  // CLAIM to be open-state numbers while holding closed-state ones. That fails the
  // run (runFailureReasons in run.js). It does not get a `.catch(() => {})`.
  try {
    await page.waitForSelector(openDef.waitFor, { timeout: 8_000 });
  } catch (err) {
    record.openFailed = `trigger "${openDef.trigger}" WAS actionable and the click landed, `
      + `but the open state "${openDef.waitFor}" never appeared within 8s `
      + `(${firstLine(err)}) -- the probes below ran against the CLOSED-state page, not `
      + `the open ${closedName}. This is not the app's 0x0-trigger bug; something else is wrong.`;
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

// triggerRect records WHY a blocked trigger was unactionable, in numbers. "0x0 at
// (0, 0)" is an actionable app finding (#2382); "44x44 but covered" is a different
// bug entirely, and the message alone cannot tell them apart. Best-effort: a
// failure to measure the trigger must not sink the record.
async function triggerRect(trigger) {
  try {
    return await trigger.evaluate(el => {
      const r = el.getBoundingClientRect();
      return {
        width: Math.round(r.width * 100) / 100,
        height: Math.round(r.height * 100) / 100,
        left: Math.round(r.left),
        top: Math.round(r.top),
      };
    });
  } catch (err) {
    return { unavailable: firstLine(err) };
  }
}
