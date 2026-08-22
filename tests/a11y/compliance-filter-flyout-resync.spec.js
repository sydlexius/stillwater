// compliance-filter-flyout-resync.spec.js - regression coverage for #3100.
//
// THE DEFECT. Dismissing a compliance filter chip (the "Non-Compliant" X in
// the toolbar row) removed the filter from the URL and the table, but the
// filter flyout's own "Non-Compliant" pill kept its server-rendered
// data-filter-selected="true" forever, because nothing ever told the flyout
// the URL had changed. DismissFilterChip (web/components/filter_flyout.templ)
// does its own history.pushState + htmx.ajax and fires no sw:filter-applied,
// and the flyout lives outside #compliance-results by design (a swap must
// never yank the panel out from under an operator), so the swap never
// refreshed it either. Pressing the flyout's own Apply afterwards read the
// stale data-filter-selected="true" pill and silently wrote status back into
// the URL, re-narrowing a report the operator had just asked to see in full.
//
// THE FIX: an htmx:afterSwap listener scoped to #compliance-results
// (repComplianceScript, web/templates/reports_page.templ) that re-runs
// swFilterFlyout.initFromURL('compliance-filter-flyout') after every swap --
// the same pattern already shipped for the artists grid and the blast-radius
// pane (see their afterSwap comments for the identical root cause).
//
// WHY /reports (the workspace route), NOT /reports/compliance. Both bugs in
// this cluster (#3099, #3100) manifest on the promoted workspace route: it
// has no fragment handler, so it is the shape under which a stale client
// state is most consequential. DismissFilterChip reloads whatever
// window.location.pathname is, so testing from the URL an operator actually
// lands on (the workspace) is what the issue is about.
//
// NO SEEDED FIXTURE NEEDED. status/filter/library_id/health_min/health_max
// are pure URL query params: complianceActiveChips and repFilterCount are
// driven entirely by ComplianceData.Status et al, and FilterItemSingle's
// selected state is computed server-side from data.Status == "non_compliant"
// -- none of it depends on whether any artist row actually matches. So this
// spec runs correctly against a bare a11y database with zero rows, and
// "?status=non_compliant" alone is enough to exercise the real defect.
//
// NEVER ASSERT A VALUE THE TEST ITSELF WROTE. The precondition below --that
// the flyout's "Non-Compliant" pill starts data-filter-selected="true" -- is
// entirely SERVER-rendered from the URL query string on first paint
// (FilterItemSingle's selected bool comes straight from data.Status). This
// spec only reads that attribute and clicks buttons; it never sets the
// attribute itself before checking it, so a broken resync cannot pass by
// accident.
//
// TAGGING THE PRE-SWAP NODE (the blast-radius spec's technique, applied
// here): a custom attribute set on the live #compliance-results node cannot
// survive an outerHTML swap, because the server's fresh markup does not
// carry it. Waiting for the tag to be GONE is how this spec knows the dismiss
// swap actually landed, rather than racing a fixed sleep against it.

import { test, expect } from 'playwright/test';

const WORKSPACE_URL = '/reports?status=non_compliant';

test('dismissing the compliance status chip clears the flyout, and Apply does not silently reinstate it', async ({ page }) => {
  await page.goto(WORKSPACE_URL);
  await page.waitForLoadState('load');
  await page.waitForSelector('#compliance-filter-trigger', { timeout: 10_000 });

  // --- PRECONDITIONS. Each one, entirely server-rendered from the URL, would
  // make the assertions below meaningless if false.
  const before = await page.evaluate(() => ({
    url: window.location.search,
    chips: document.querySelectorAll('[aria-label^="Remove "]').length,
    statusSelected: document.querySelectorAll(
      '#compliance-filter-flyout [data-filter-key="status"][data-filter-value="non_compliant"][data-filter-selected="true"]',
    ).length,
    triggerBadgeText: (document.querySelector('#compliance-filter-trigger .sw-filter-trigger-badge') || {})
      .textContent ?? null,
  }));
  expect(before.url, 'the status query param did not survive navigation; nothing is set up to dismiss')
    .toContain('status=non_compliant');
  expect(before.chips, 'no dismissable chip is rendered for ?status=non_compliant, so there is nothing to dismiss')
    .toBe(1);
  expect(
    before.statusSelected,
    'the flyout\'s Non-Compliant pill was not server-rendered as selected for ?status=non_compliant; the '
    + 'precondition this spec depends on does not hold',
  ).toBe(1);
  expect(before.triggerBadgeText, 'the trigger badge did not report the active filter before dismiss')
    .toBe('1');

  // --- Tag the pre-dismiss results shell so the wait below cannot pass
  // before the swap actually lands.
  await page.evaluate(() => {
    const el = document.getElementById('compliance-results');
    if (el) el.setAttribute('data-sw-preswap', '1');
  });
  expect(
    await page.evaluate(() => document.querySelectorAll('#compliance-results[data-sw-preswap]').length),
    'the pre-swap results shell could not be tagged, so the swap below cannot be detected',
  ).toBe(1);

  // --- Dismiss the chip. The dismiss button lives in the always-visible
  // toolbar row, not inside the flyout panel, so no need to open the flyout
  // first.
  await page.locator('[aria-label^="Remove "]').first().click();

  // The wait IS the assertion that a swap happened at all: a fixed sleep
  // could observe the pre-fix DOM mid-flight and pass vacuously.
  await page.waitForFunction(
    () => !document.querySelector('#compliance-results[data-sw-preswap]'),
    null,
    { timeout: 15_000 },
  );

  // --- POST-DISMISS: the flyout resynced.
  const after = await page.evaluate(() => ({
    url: window.location.search,
    statusSelected: document.querySelectorAll(
      '#compliance-filter-flyout [data-filter-key="status"][data-filter-value="non_compliant"][data-filter-selected="true"]',
    ).length,
    footerText: (document.querySelector('#compliance-filter-flyout .sw-filter-active-badge') || {})
      .textContent?.trim() ?? '',
  }));
  expect(after.url, 'the status query param survived the dismiss; the chip did not actually clear it')
    .not.toContain('status=');
  expect(
    after.statusSelected,
    'the flyout still shows Non-Compliant as selected after its chip was dismissed; the panel is stale and '
    + 'its own Apply would silently re-apply the filter the operator just cleared',
  ).toBe(0);
  expect(after.footerText, 'the flyout footer still claims an active filter after the only one was cleared')
    .toBe('');
  await expect(
    page.locator('#compliance-filter-trigger .sw-filter-trigger-badge'),
    'the trigger badge is still visible after the only active filter was dismissed',
  ).toBeHidden();

  // --- THE ASSERTION THE ISSUE IS ACTUALLY ABOUT: pressing Apply right after
  // the dismiss must not silently reinstate the filter the operator just
  // cleared. Open the flyout (Apply lives inside it and Playwright will not
  // click through an inert panel), then press it.
  await page.locator('#compliance-filter-trigger').click();
  await page.waitForSelector('#compliance-filter-flyout:not([inert])', { timeout: 5_000 });
  await page.locator('#compliance-filter-flyout .sw-filter-btn-primary').click();

  // apply() (filter-flyout.js) calls history.pushState synchronously before
  // triggering the async htmx reload, so the URL is the reliable, immediate
  // signal of what Apply actually decided to write -- no need to intercept
  // the network request it fires afterward.
  await expect
    .poll(
      async () => page.evaluate(() => window.location.search),
      {
        message: 'Apply wrote status back into the URL: the flyout was still stale when Apply ran, and it '
        + 're-applied a filter the operator had just dismissed',
        timeout: 5_000,
      },
    )
    .not.toContain('status=');
});
